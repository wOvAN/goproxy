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
	"github.com/goproxyio/goproxy/v2/sumdb"
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
		sumdb.Handler(mw, r)
		rt.count("sumdb", mw)
		return
	}

	if rt.proxy == nil || rt.Direct(strings.TrimPrefix(r.URL.Path, "/")) {
		log.Printf("------ --- %s [direct]\n", r.URL)
		rt.srv.ServeHTTP(mw, r)
		rt.count("direct", mw)
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
					rt.count("proxy", mw)
				} else {
					ctype = "text/plain; charset=UTF-8"
					mw.Header().Set("Content-Type", ctype)
					http.ServeContent(mw, r, "", info.ModTime(), f)
					rt.count("cached", mw)
				}
				return
			}

			i := strings.Index(r.URL.Path, "/@v/")
			if i < 0 {
				http.Error(mw, "no such path", http.StatusNotFound)
				rt.count("cached", mw)
				return
			}

			what := r.URL.Path[i+len("/@v/"):]
			if what == "list" {
				if time.Since(info.ModTime()) >= rt.cacheExpire {
					log.Printf("------ --- %s [proxy]\n", r.URL)
					rt.proxy.ServeHTTP(mw, r)
					rt.count("proxy", mw)
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
					rt.count("cached", mw)
					return
				}
			}
			mw.Header().Set("Content-Type", ctype)
			http.ServeContent(mw, r, "", info.ModTime(), f)
			rt.count("cached", mw)
			return
		}
	}
	log.Printf("------ --- %s [proxy]\n", r.URL)
	rt.proxy.ServeHTTP(mw, r)
	rt.count("proxy", mw)
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
