package proxy

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/goproxyio/goproxy/v2/renameio"
	"github.com/goproxyio/goproxy/v2/sumdb"

	"github.com/prometheus/client_golang/prometheus"
)

// ListExpire list data expire data duration.
const ListExpire = 5 * time.Minute

// RouterOptions provides the proxy host and the external pattern
type RouterOptions struct {
	Pattern      string
	Proxy        string
	DownloadRoot string
	CacheExpire  time.Duration
}

// A Router is the proxy HTTP server,
// which implements Route Filter to
// routing private module or public module .
type Router struct {
	opts         *RouterOptions
	srv          *Server
	proxy        *httputil.ReverseProxy
	pattern      string
	downloadRoot string
	cacheExpire  time.Duration
}

func (router *Router) customModResponse(r *http.Response) error {
	// Only module files may be stored in the download cache.
	if p := r.Request.URL.Path; !strings.Contains(p, "/@v/") && !strings.HasSuffix(p, "/@latest") {
		return nil
	}
	if r.StatusCode == http.StatusOK {
		file := filepath.Join(router.opts.DownloadRoot, r.Request.URL.Path)
		return cacheResponseBody(r, file)
	}
	// support 302 status code.
	if r.StatusCode == http.StatusFound {
		loc := r.Header.Get("Location")
		if loc == "" {
			return fmt.Errorf("%d response missing Location header", r.StatusCode)
		}
		u, err := url.Parse(loc)
		if err != nil {
			return fmt.Errorf("failed to parse Location header %q: %v", loc, err)
		}
		resp, err := http.Get(r.Request.URL.ResolveReference(u).String())
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			// Do not cache the body of an error response as a module file.
			return nil
		}
		file := filepath.Join(router.opts.DownloadRoot, r.Request.URL.Path)
		return cacheResponseBody(resp, file)
	}
	return nil
}

func cacheResponseBody(resp *http.Response, file string) error {
	var buf []byte
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer func() { _ = gr.Close() }()
		buf, err = io.ReadAll(gr)
		if err != nil {
			return err
		}
		resp.Header.Del("Content-Encoding")
		resp.Header.Set("Content-Length", fmt.Sprint(len(buf)))
	} else {
		var err error
		buf, err = io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	if buf != nil {
		if err := os.MkdirAll(filepath.ToSlash(filepath.Dir(file)), os.ModePerm); err != nil {
			return err
		}
		return renameio.WriteFile(file, buf, 0666)
	}
	return nil
}

// NewRouter returns a new Router using the given operations.
func NewRouter(srv *Server, opts *RouterOptions) *Router {
	rt := &Router{
		opts: opts,
		srv:  srv,
	}
	if opts != nil {
		if opts.Proxy == "" {
			log.Printf("not set proxy, all direct.")
			return rt
		}
		remote, err := url.Parse(opts.Proxy)
		if err != nil {
			log.Printf("parse proxy fail, all direct.")
			return rt
		}
		proxy := httputil.NewSingleHostReverseProxy(remote)
		director := proxy.Director               //nolint:staticcheck // Director is deprecated; Rewrite would drop the single-host joinURL rewrite
		proxy.Director = func(r *http.Request) { //nolint:staticcheck
			director(r)
			r.Host = remote.Host
		}

		rt.proxy = proxy

		rt.proxy.Transport = &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		rt.proxy.ModifyResponse = rt.customModResponse
		rt.pattern = opts.Pattern
		rt.downloadRoot = opts.DownloadRoot
		rt.cacheExpire = opts.CacheExpire
	}
	return rt
}

// Direct decides whether a path should directly access.
func (rt *Router) Direct(path string) bool {
	if rt.pattern == "" {
		return false
	}
	return GlobsMatchPath(rt.pattern, path)
}

// ServveHTTP implements http handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mw := NewMetricsResponseWriter(w)
	// sumdb handler
	if strings.HasPrefix(r.URL.Path, "/sumdb/") {
		sumdb.Handler(mw, r)
		totalRequest.With(prometheus.Labels{"mode": "sumdb", "status": mw.status()}).Inc()
		return
	}

	if rt.proxy == nil || rt.Direct(strings.TrimPrefix(r.URL.Path, "/")) {
		log.Printf("------ --- %s [direct]\n", r.URL)
		rt.srv.ServeHTTP(mw, r)
		totalRequest.With(prometheus.Labels{"mode": "direct", "status": mw.status()}).Inc()
		return
	}

	file := filepath.Join(rt.downloadRoot, r.URL.Path)
	if info, err := os.Stat(file); err == nil {
		if f, err := os.Open(file); err == nil {
			var ctype string
			defer func() { _ = f.Close() }()
			if strings.HasSuffix(r.URL.Path, "/@latest") {
				if time.Since(info.ModTime()) >= ListExpire {
					log.Printf("------ --- %s [proxy]\n", r.URL)
					rt.proxy.ServeHTTP(mw, r)
					totalRequest.With(prometheus.Labels{"mode": "proxy", "status": mw.status()}).Inc()
				} else {
					ctype = "text/plain; charset=UTF-8"
					mw.Header().Set("Content-Type", ctype)
					log.Printf("------ --- %s [cached]\n", r.URL)
					http.ServeContent(mw, r, "", info.ModTime(), f)
					totalRequest.With(prometheus.Labels{"mode": "cached", "status": mw.status()}).Inc()
				}
				return
			}

			i := strings.Index(r.URL.Path, "/@v/")
			if i < 0 {
				http.Error(mw, "no such path", http.StatusNotFound)
				totalRequest.With(prometheus.Labels{"mode": "cached", "status": mw.status()}).Inc()
				return
			}

			what := r.URL.Path[i+len("/@v/"):]
			if what == "list" {
				if time.Since(info.ModTime()) >= rt.cacheExpire {
					log.Printf("------ --- %s [proxy]\n", r.URL)
					rt.proxy.ServeHTTP(mw, r)
					totalRequest.With(prometheus.Labels{"mode": "proxy", "status": mw.status()}).Inc()
					return
				}
				ctype = "text/plain; charset=UTF-8"
			} else {
				ext := path.Ext(what)
				switch ext {
				case ".info":
					ctype = "application/json"
				case ".mod":
					ctype = "text/plain; charset=UTF-8"
				case ".zip":
					ctype = "application/octet-stream"
				default:
					http.Error(mw, "request not recognized", http.StatusNotFound)
					totalRequest.With(prometheus.Labels{"mode": "cached", "status": mw.status()}).Inc()
					return
				}
			}
			mw.Header().Set("Content-Type", ctype)
			log.Printf("------ --- %s [cached]\n", r.URL)
			http.ServeContent(mw, r, "", info.ModTime(), f)
			totalRequest.With(prometheus.Labels{"mode": "cached", "status": mw.status()}).Inc()
			return
		}
	}
	log.Printf("------ --- %s [proxy]\n", r.URL)
	rt.proxy.ServeHTTP(mw, r)
	totalRequest.With(prometheus.Labels{"mode": "proxy", "status": mw.status()}).Inc()
}

// GlobsMatchPath reports whether any path prefix of target
// matches one of the glob patterns (as defined by path.Match)
// in the comma-separated globs list.
// It ignores any empty or malformed patterns in the list.
func GlobsMatchPath(globs, target string) bool {
	for globs != "" {
		// Extract next non-empty glob in comma-separated list.
		var glob string
		if i := strings.Index(globs, ","); i >= 0 {
			glob, globs = globs[:i], globs[i+1:]
		} else {
			glob, globs = globs, ""
		}
		if glob == "" {
			continue
		}

		// A glob with N+1 path elements (N slashes) needs to be matched
		// against the first N+1 path elements of target,
		// which end just before the N+1'th slash.
		n := strings.Count(glob, "/")
		prefix := target
		// Walk target, counting slashes, truncating at the N+1'th slash.
		for i := 0; i < len(target); i++ {
			if target[i] == '/' {
				if n == 0 {
					prefix = target[:i]
					break
				}
				n--
			}
		}
		if n > 0 {
			// Not enough prefix elements.
			continue
		}
		matched, _ := path.Match(glob, prefix)
		if matched {
			return true
		}
	}
	return false
}
