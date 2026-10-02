package proxy

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/goproxyio/goproxy/v2/renameio"
)

// ListExpire list data expire data duration.
const ListExpire = 5 * time.Minute

// RouterOptions provides the proxy host and the external pattern
type RouterOptions struct {
	Pattern      string
	Proxy        string
	DownloadRoot string
	CacheExpire  time.Duration
	// Sumdb serves /sumdb/... requests.
	Sumdb http.Handler
	// DisableModuleFetch serves from cache only, globally.
	DisableModuleFetch bool
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
	disableFetch bool
}

// fetchDisabled reports whether this request must be answered from the
// cache only (global flag or Disable-Module-Fetch request header).
func (rt *Router) fetchDisabled(r *http.Request) bool {
	return rt.disableFetch || r.Header.Get(HeaderDisableModuleFetch) == "true"
}

func (router *Router) customModResponse(r *http.Response) error {
	// Only module files may be stored in the download cache.
	if p := r.Request.URL.Path; !strings.Contains(p, "/@v/") && !strings.HasSuffix(p, "/@latest") {
		return nil
	}
	if r.StatusCode == http.StatusOK {
		file := filepath.Join(router.opts.DownloadRoot, r.Request.URL.Path)
		if err := cacheResponseBody(r, file); err != nil {
			return err
		}
		if cc := cacheControlFor(r.Request.URL.Path); cc != "" {
			r.Header.Set("Cache-Control", cc)
		}
		return nil
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
		if err := cacheResponseBody(resp, file); err != nil {
			return err
		}
		if cc := cacheControlFor(r.Request.URL.Path); cc != "" {
			r.Header.Set("Cache-Control", cc)
		}
		return nil
	}
	return nil
}

// cacheResponseBody streams the response body into the download cache, then
// replaces the response body with the cached file so the client is served from
// the same stream. The body is never fully buffered in memory.
func cacheResponseBody(resp *http.Response, file string) error {
	var src io.Reader = resp.Body
	compressed := strings.Contains(resp.Header.Get("Content-Encoding"), "gzip")
	if compressed {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer func() { _ = gr.Close() }()
		src = gr
	}
	cr := &countReader{r: src}
	if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
		return err
	}
	if err := renameio.WriteToFile(file, cr, 0666); err != nil {
		return err
	}
	// The original body is fully consumed; release its connection before
	// replacing it with the cached file.
	_ = resp.Body.Close()
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	resp.Body = f
	if compressed {
		resp.Header.Del("Content-Encoding")
		resp.Header.Set("Content-Length", strconv.FormatInt(cr.n, 10))
	}
	return nil
}

// countReader reads from r while counting the bytes read.
type countReader struct {
	r io.Reader
	n int64
}

// Read implements io.Reader.
func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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

		rt.proxy.ModifyResponse = rt.customModResponse
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		transport.MaxIdleConnsPerHost = 100
		transport.ResponseHeaderTimeout = 30 * time.Second
		rt.proxy.Transport = transport
		rt.pattern = opts.Pattern
		rt.downloadRoot = opts.DownloadRoot
		rt.cacheExpire = opts.CacheExpire
		rt.disableFetch = opts.DisableModuleFetch
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

// count increments the request counter. WithLabelValues avoids the
// prometheus.Labels map allocation on the hot path.
func (rt *Router) count(mode string, mw *metricsResponseWriter) {
	totalRequest.WithLabelValues(mode, mw.status()).Inc()
}

// ServveHTTP implements http handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mw := NewMetricsResponseWriter(w)
	// sumdb handler
	if strings.HasPrefix(r.URL.Path, "/sumdb/") {
		if rt.srv.sumdb == nil {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte("unsupported db\n"))
			rt.count("sumdb", mw)
			return
		}
		rt.srv.sumdb.ServeHTTP(mw, r)
		rt.count("sumdb", mw)
		return
	}

	if rt.fetchDisabled(r) {
		log.Printf("------ --- %s [cache-only]\n", r.URL)
		mw.Header().Set(HeaderDisableModuleFetch, "true")
		if _, served := rt.serveFromCache(mw, r, true); served {
			rt.count("cached", mw)
			return
		}
		http.Error(mw, ErrFetchDisabled.Error(), http.StatusGone)
		rt.count("cached", mw)
		return
	}

	if rt.proxy == nil || rt.Direct(strings.TrimPrefix(r.URL.Path, "/")) {
		log.Printf("------ --- %s [direct]\n", r.URL)
		rt.srv.ServeHTTP(mw, r)
		rt.count("direct", mw)
		return
	}

	if _, served := rt.serveFromCache(mw, r, false); served {
		rt.count("cached", mw)
		return
	}
	log.Printf("------ --- %s [proxy]\n", r.URL)
	rt.proxy.ServeHTTP(mw, r)
	rt.count("proxy", mw)
}

// contentTypeFor returns the content type for a module file served from
// the download cache, or "" if the path does not name a module file.
func contentTypeFor(p string) string {
	if strings.HasSuffix(p, "/@latest") {
		return "text/plain; charset=UTF-8"
	}
	i := strings.Index(p, "/@v/")
	if i < 0 {
		return ""
	}
	what := p[i+len("/@v/"):]
	if what == "list" {
		return "text/plain; charset=UTF-8"
	}
	switch path.Ext(what) {
	case ".info":
		return "application/json"
	case ".mod":
		return "text/plain; charset=UTF-8"
	case ".zip":
		return "application/octet-stream"
	}
	return ""
}

// serveFromCache serves the cached copy of the module file requested by r.
// found reports that a cache file exists; served reports that a response
// was written. When bypassTTL is true, list and @latest files are served
// even past their cache expiry (cache-only mode: nothing to refetch from).
func (rt *Router) serveFromCache(mw *metricsResponseWriter, r *http.Request, bypassTTL bool) (found, served bool) {
	file := filepath.Join(rt.downloadRoot, r.URL.Path)
	info, err := os.Stat(file)
	if err != nil {
		return false, false
	}
	f, err := os.Open(file)
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()
	found = true
	if !bypassTTL {
		what := ""
		if i := strings.Index(r.URL.Path, "/@v/"); i >= 0 {
			what = r.URL.Path[i+len("/@v/"):]
		}
		if strings.HasSuffix(r.URL.Path, "/@latest") && time.Since(info.ModTime()) >= ListExpire {
			return true, false
		}
		if what == "list" && time.Since(info.ModTime()) >= rt.cacheExpire {
			return true, false
		}
	}
	ctype := contentTypeFor(r.URL.Path)
	if ctype == "" {
		http.Error(mw, "request not recognized", http.StatusNotFound)
		return true, true
	}
	mw.Header().Set("Content-Type", ctype)
	if cc := cacheControlFor(r.URL.Path); cc != "" {
		mw.Header().Set("Cache-Control", cc)
	}
	http.ServeContent(mw, r, "", info.ModTime(), f)
	return true, true
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
