// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Usage:
//
//	goproxy [-listen [host]:port] [-cacheDir /tmp]
//
// goproxy serves the Go module proxy HTTP protocol at the given address (default 0.0.0.0:8081).
// It invokes the local go command to answer requests and therefore reuses
// the current GOPATH's module download cache and configuration (GOPROXY, GOSUMDB, and so on).
//
// While the proxy is running, setting GOPROXY=http://host:port will instruct the go command to use it.
// Note that the module proxy cannot share a GOPATH with its own clients or else fetches will deadlock.
// (The client will lock the entry as “being downloaded” before sending the request to the proxy,
// which will then wait for the apparently-in-progress download to finish.)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"

	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goproxyio/goproxy/v2/proxy"
	"github.com/goproxyio/goproxy/v2/renameio"
	"github.com/goproxyio/goproxy/v2/sumdb"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/mod/module"
)

var downloadRoot string
var listen string
var cacheDir string
var proxyHost string
var sumdbProxy string
var excludeHost string
var cacheExpire time.Duration
var disableModuleFetch bool
var gcInterval time.Duration
var gcKeep time.Duration

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// setup parses flags and prepares the environment for the go command.
// It runs from main (not init) so test binaries are not polluted with flag parsing.
func setup() {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&excludeHost, "exclude", "", "exclude host pattern, you can exclude internal Git services")
	flag.StringVar(&proxyHost, "proxy", "", "next hop proxy for Go Modules, recommend use https://goproxy.io")
	flag.StringVar(&sumdbProxy, "sumdbProxy", "", "sumdb proxy host; empty (default) races the built-in sumdb mirrors directly")
	flag.StringVar(&cacheDir, "cacheDir", "", "Go Modules cache dir, default is $GOPATH/pkg/mod/cache/download")
	flag.StringVar(&listen, "listen", "0.0.0.0:8081", "service listen address")
	flag.DurationVar(&cacheExpire, "cacheExpire", 5*time.Minute, "Go Modules cache expiration (min), default is 5 min")
	flag.BoolVar(&disableModuleFetch, "disableModuleFetch", false, "serve modules and sumdb from the cache only; never fetch upstream")
	flag.DurationVar(&gcInterval, "gcInterval", 0, "cache garbage collection interval; a cache file untouched for gcKeep is deleted; 0 disables GC")
	flag.DurationVar(&gcKeep, "gcKeep", 14*24*time.Hour, "cache file age (since last access) to keep during GC")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if os.Getenv("GIT_TERMINAL_PROMPT") == "" {
		_ = os.Setenv("GIT_TERMINAL_PROMPT", "0")
	}

	if os.Getenv("GIT_SSH") == "" && os.Getenv("GIT_SSH_COMMAND") == "" {
		_ = os.Setenv("GIT_SSH_COMMAND", "ssh -o ControlMaster=no")
	}

	if excludeHost != "" {
		_ = os.Setenv("GOPRIVATE", excludeHost)
	}

	// Enable Go module
	_ = os.Setenv("GO111MODULE", "on")
	_ = os.Setenv("GOPROXY", "direct")
	_ = os.Setenv("GOSUMDB", "off")

	downloadRoot = getDownloadRoot()
}

func main() {
	setup()
	log.SetPrefix("goproxy.io: ")
	log.SetFlags(0)
	log.Printf("version %s\n", version)

	var handle http.Handler

	if sumdbProxy != "" {
		log.Printf("SumDBProxy %s\n", sumdbProxy)
		sumdb.SetSumdbProxy(sumdbProxy)
	}
	if disableModuleFetch {
		log.Println("module fetch disabled: serving from cache only")
	}
	sumdbHandler := sumdb.NewHandler(downloadRoot, disableModuleFetch)

	if proxyHost != "" {
		log.Printf("ProxyHost %s\n", proxyHost)
		if excludeHost != "" {
			log.Printf("ExcludeHost %s\n", excludeHost)
		}
		handle = &logger{proxy.NewRouter(proxy.NewServer(new(ops), sumdbHandler), &proxy.RouterOptions{
			Pattern:            excludeHost,
			Proxy:              proxyHost,
			DownloadRoot:       downloadRoot,
			CacheExpire:        cacheExpire,
			Sumdb:              sumdbHandler,
			DisableModuleFetch: disableModuleFetch,
		})}
	} else {
		handle = &logger{proxy.MetricsMiddleware("direct", proxy.NewServer(new(ops), sumdbHandler))}
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           handle,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}
	}()
	if gcInterval > 0 {
		go runCacheGC()
	}

	s := make(chan os.Signal, 1)
	signal.Notify(s, os.Interrupt, syscall.SIGTERM)
	<-s
	log.Println("Making a graceful shutdown...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := server.Shutdown(ctx)
	if err != nil {
		log.Fatalf("Error while shutting down the server: %v", err)
	}
	log.Println("Successful server shutdown.")
}

func getDownloadRoot() string {
	var env struct {
		GOPATH string
	}
	if cacheDir != "" {
		_ = os.Setenv("GOMODCACHE", filepath.Join(cacheDir, "pkg", "mod"))
		return filepath.Join(cacheDir, "pkg", "mod", "cache", "download")
	}
	if err := goJSON(&env, "go", "env", "-json", "GOPATH"); err != nil {
		log.Fatal(err)
	}
	list := filepath.SplitList(env.GOPATH)
	if len(list) == 0 || list[0] == "" {
		log.Fatalf("missing $GOPATH")
	}
	_ = os.Setenv("GOMODCACHE", filepath.Join(list[0], "pkg", "mod"))
	return filepath.Join(list[0], "pkg", "mod", "cache", "download")
}

// goNotFoundPatterns are substrings of go command diagnostics that mean
// "the module or version does not exist" as opposed to a transient failure.
var goNotFoundPatterns = []string{
	"no matching versions",
	"unknown revision",
	"cannot find module",
	"malformed module path",
}

// mapNotFound tags go command errors that indicate a missing module with
// fs.ErrNotExist so that the proxy can answer 404 instead of 500.
func mapNotFound(err error) error {
	msg := err.Error()
	for _, p := range goNotFoundPatterns {
		if strings.Contains(msg, p) {
			return fmt.Errorf("%w: %s", fs.ErrNotExist, msg)
		}
	}
	return err
}

// goJSON runs the go command and parses its JSON output into dst.
func goJSON(dst any, command ...string) error {
	goCmdSem <- struct{}{}
	defer func() { <-goCmdSem }()
	cmd := exec.Command(command[0], command[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return mapNotFound(fmt.Errorf("%s:\n%s%s", strings.Join(command, " "), stderr.String(), stdout.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), dst); err != nil {
		return fmt.Errorf("%s: reading json: %v", strings.Join(command, " "), err)
	}
	return nil
}

// A logger is an http.Handler that logs traffic to standard error.
type logger struct {
	h http.Handler
}
type responseLogger struct {
	code int
	http.ResponseWriter
}

// WriteHeader writes header code into responser writer.
func (r *responseLogger) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// ServeHTTP implements http handler.
func (l *logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	// Prometheus metrics
	if r.URL.Path == "/metrics" {
		promhttp.Handler().ServeHTTP(w, r)
		return
	}

	start := time.Now()
	rl := &responseLogger{code: 200, ResponseWriter: w}
	l.h.ServeHTTP(rl, r)
	log.Printf("%.3fs %d %s\n", time.Since(start).Seconds(), rl.code, r.URL)
}

// An ops is a proxy.ServerOps implementation.
type ops struct{}

// NewContext creates a context.
func (*ops) NewContext(r *http.Request) (context.Context, error) {
	ctx := context.Background()
	if disableModuleFetch || r.Header.Get(proxy.HeaderDisableModuleFetch) == "true" {
		ctx = proxy.WithFetchDisabled(ctx, true)
	}
	return ctx, nil
}

// List lists proxy files. In cache-only mode it serves the cached list
// file regardless of its age and never runs the go command.
func (*ops) List(ctx context.Context, mpath string) (proxy.File, error) {
	file := listPath(mpath)
	if proxy.FetchDisabled(ctx) {
		return openCached(file)
	}
	if info, err := os.Stat(file); err == nil && time.Since(info.ModTime()) < cacheExpire {
		return os.Open(file)
	}
	data, err := fetchList(mpath)
	if err != nil {
		return nil, err
	}
	return proxy.MemFile(data, time.Now()), nil
}

// openCached opens a download-cache file, mapping any failure to
// ErrFetchDisabled so the server answers 410 in cache-only mode.
func openCached(file string) (proxy.File, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", proxy.ErrFetchDisabled, file)
	}
	return f, nil
}

// fetchList resolves the version list for mpath, merging concurrent requests
// for the same module into one go command run.
func fetchList(mpath string) ([]byte, error) {
	v, err := doOnce("list:"+mpath, func() (any, error) {
		var list struct {
			Path     string
			Versions []string
		}
		if err := goJSON(&list, "go", "list", "-m", "-json", "-versions", mpath+"@latest"); err != nil {
			return nil, err
		}
		if list.Path != mpath {
			return nil, fmt.Errorf("go list -m: asked for %s but got %s", mpath, list.Path)
		}
		data := []byte(strings.Join(list.Versions, "\n") + "\n")
		if len(data) == 1 {
			data = nil
		}
		file := listPath(mpath)
		if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
			log.Printf("make cache dir failed, err: %v.", err)
			return nil, err
		}
		if err := renameio.WriteFile(file, data, 0666); err != nil {
			return nil, err
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// Latest fetches latest file. In cache-only mode it resolves the latest
// known version from the cached list file and serves its cached info file.
func (*ops) Latest(ctx context.Context, path string) (proxy.File, error) {
	if proxy.FetchDisabled(ctx) {
		return latestFromCache(path)
	}
	d, err := download(module.Version{Path: path, Version: "latest"})
	if err != nil {
		return nil, err
	}
	return os.Open(d.Info)
}

// latestFromCache answers @latest from the cached version list, without
// running the go command.
func latestFromCache(mpath string) (proxy.File, error) {
	data, err := os.ReadFile(listPath(mpath))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", proxy.ErrFetchDisabled, listPath(mpath))
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var latest string
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			latest = lines[i]
			break
		}
	}
	if latest == "" {
		return nil, fmt.Errorf("%w: empty list for %s", proxy.ErrFetchDisabled, mpath)
	}
	f, err := cachedFile(module.Version{Path: mpath, Version: latest}, ".info")
	if err != nil {
		return nil, fmt.Errorf("%w: %s@%s", proxy.ErrFetchDisabled, mpath, latest)
	}
	return f, nil
}

// Info fetches info file.
func (*ops) Info(ctx context.Context, m module.Version) (proxy.File, error) {
	if f, err := cachedFile(m, ".info"); err == nil {
		return f, nil
	}
	if proxy.FetchDisabled(ctx) {
		return nil, fmt.Errorf("%w: %s", proxy.ErrFetchDisabled, m)
	}
	d, err := download(m)
	if err != nil {
		return nil, err
	}
	return os.Open(d.Info)
}

// GoMod fetches go mod file.
func (*ops) GoMod(ctx context.Context, m module.Version) (proxy.File, error) {
	if f, err := cachedFile(m, ".mod"); err == nil {
		return f, nil
	}
	if proxy.FetchDisabled(ctx) {
		return nil, fmt.Errorf("%w: %s", proxy.ErrFetchDisabled, m)
	}
	d, err := download(m)
	if err != nil {
		return nil, err
	}
	return os.Open(d.GoMod)
}

// Zip fetches zip file.
func (*ops) Zip(ctx context.Context, m module.Version) (proxy.File, error) {
	if f, err := cachedFile(m, ".zip"); err == nil {
		return f, nil
	}
	if proxy.FetchDisabled(ctx) {
		return nil, fmt.Errorf("%w: %s", proxy.ErrFetchDisabled, m)
	}
	d, err := download(m)
	if err != nil {
		return nil, err
	}
	return os.Open(d.Zip)
}

type downloadInfo struct {
	Path     string
	Version  string
	Info     string
	GoMod    string
	Zip      string
	Dir      string
	Sum      string
	GoModSum string
}

// cachedFile opens the module's download cache file with the given extension,
// so repeated requests never spawn a go command.
func cachedFile(m module.Version, ext string) (proxy.File, error) {
	escMod, err := module.EscapePath(m.Path)
	if err != nil {
		return nil, err
	}
	escVer, err := module.EscapeVersion(m.Version)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(downloadRoot, escMod, "@v", escVer+ext))
}

// listPath returns the download cache list file path for a module.
func listPath(mpath string) string {
	escMod, _ := module.EscapePath(mpath)
	return filepath.Join(downloadRoot, escMod, "@v", "list")
}

// goCmdSem bounds concurrent go command subprocesses so a cold-cache
// request storm cannot spawn hundreds of simultaneous git clones.
var goCmdSem = make(chan struct{}, 2*runtime.NumCPU())

// flights merges concurrent fetches of the same key into one run.
var flights = struct {
	sync.Mutex
	m map[string]*flight
}{m: map[string]*flight{}}

type flight struct {
	wg  sync.WaitGroup
	val any
	err error
}

// doOnce runs f once per concurrent call with the same key and shares its
// result with all waiting callers.
func doOnce(key string, f func() (any, error)) (any, error) {
	flights.Lock()
	if fl, ok := flights.m[key]; ok {
		flights.Unlock()
		fl.wg.Wait()
		return fl.val, fl.err
	}
	fl := &flight{}
	fl.wg.Add(1)
	flights.m[key] = fl
	flights.Unlock()
	fl.val, fl.err = f()
	flights.Lock()
	delete(flights.m, key)
	flights.Unlock()
	fl.wg.Done()
	return fl.val, fl.err
}

func download(m module.Version) (*downloadInfo, error) {
	v, err := doOnce(m.String(), func() (any, error) {
		d := new(downloadInfo)
		return d, goJSON(d, "go", "mod", "download", "-json", m.String())
	})
	if err != nil {
		return nil, err
	}
	return v.(*downloadInfo), nil
}
