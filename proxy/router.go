package proxy

import (
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goproxyio/goproxy/v2/logger"
	"github.com/goproxyio/goproxy/v2/renameio"

	"golang.org/x/mod/module"
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
	// DialTimeout bounds establishing upstream connections (0: library default).
	DialTimeout time.Duration
	// Insecure skips upstream TLS certificate verification.
	Insecure bool
	// TempDir holds stream-through upstream copies ("" = os.TempDir).
	TempDir string
}

// A Router is the proxy HTTP server,
// which implements Route Filter to
// routing private module or public module .
type Router struct {
	opts         *RouterOptions
	srv          *Server
	proxy        *httputil.ReverseProxy
	chain        []upstreamStep
	tail         string
	chainClient  *http.Client
	pattern      string
	downloadRoot string
	cacheExpire  time.Duration
	disableFetch bool
	tempDir      string
}

// fetchDisabled reports whether this request must be answered from the
// cache only (global flag or Disable-Module-Fetch request header).
func (rt *Router) fetchDisabled(r *http.Request) bool {
	v, _ := strconv.ParseBool(r.Header.Get(HeaderDisableModuleFetch))
	return rt.disableFetch || v
}

// CacheRestricted reports whether an upstream response forbids being cached:
// revalidation-required or private Cache-Control, an expired max-age, or a
// Vary: * response. max-age is matched as a whole token so that max-age=60 is
// not confused with max-age=0; quoted directive arguments are not parsed
// (ponytail: upstreams do not quote them in practice).
func CacheRestricted(h http.Header) bool {
	cc := strings.ToLower(strings.Join(h.Values("Cache-Control"), ","))
	for _, d := range []string{"no-store", "no-cache", "must-revalidate", "private", "proxy-revalidate", "s-maxage"} {
		if strings.Contains(cc, d) {
			return true
		}
	}
	for tok := range strings.SplitSeq(cc, ",") {
		if strings.TrimSpace(tok) == "max-age=0" {
			return true
		}
	}
	for _, v := range h.Values("Vary") {
		for name := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(name) == "*" {
				return true
			}
		}
	}
	return false
}

// cacheFileFor returns the download-cache file for a request URL path,
// built from a rooted-cleaned path so ".." segments cannot escape the root.
func cacheFileFor(downloadRoot, urlPath string) string {
	// Store version-named files under the canonical, escaped version (the
	// layout the go command's download cache uses), so mixed-case and
	// unescaped variants of one version map to one cache file.
	if i := strings.LastIndex(urlPath, "/@v/"); i >= 0 {
		what := urlPath[i+len("/@v/"):]
		if j := strings.LastIndex(what, "."); j > 0 {
			v := what[:j]
			if dec, err := module.UnescapeVersion(v); err == nil {
				v = dec
			}
			// CanonicalVersion only understands the lowercase "v" prefix.
			if strings.HasPrefix(v, "V") {
				v = "v" + v[1:]
			}
			if esc, err := module.EscapeVersion(module.CanonicalVersion(v)); err == nil {
				urlPath = urlPath[:i+len("/@v/")] + esc + what[j:]
			}
		}
	}
	return filepath.Join(downloadRoot, filepath.FromSlash(path.Clean("/"+urlPath)))
}

// isModuleFile reports whether p names a servable module file.
func isModuleFile(p string) bool {
	return strings.Contains(p, "/@v/") || strings.HasSuffix(p, "/@latest")
}

func (router *Router) customModResponse(r *http.Response) error {
	p := r.Request.URL.Path
	// Only module files may be stored in the download cache.
	if !isModuleFile(p) {
		return nil
	}
	file := cacheFileFor(router.opts.DownloadRoot, p)
	if r.StatusCode == http.StatusOK {
		// An upstream response that forbids caching is passed through
		// untouched, never written to the download cache.
		if CacheRestricted(r.Header) {
			return nil
		}
		if err := cacheResponseBody(r, file); err != nil {
			return err
		}
		if cc := cacheControlFor(p); cc != "" {
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
		resp, err := router.chainClient.Get(r.Request.URL.ResolveReference(u).String())
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			// Do not cache the body of an error response as a module file;
			// drain a bounded amount so the connection can be reused.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			return router.serveStaleOnError(r, file, p)
		}
		if err := cacheResponseBody(resp, file); err != nil {
			return err
		}
		if cc := cacheControlFor(p); cc != "" {
			r.Header.Set("Cache-Control", cc)
		}
		return nil
	}
	// Upstream reported a miss/error: fall back to a stale cached copy.
	return router.serveStaleOnError(r, file, p)
}

// serveStaleOnError replaces a non-success upstream module-file response with
// the cached copy of that file, if one exists (stale-on-error). It never
// caches the upstream error body.
func (router *Router) serveStaleOnError(r *http.Response, file, p string) error {
	f, err := os.Open(file)
	if err != nil {
		return nil // no cache: pass the upstream status through unchanged.
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		return nil
	}
	_ = r.Body.Close()
	r.Body = f
	r.StatusCode = http.StatusOK
	r.ContentLength = info.Size()
	r.Header.Del("Content-Encoding")
	if cc := cacheControlFor(p); cc != "" {
		r.Header.Set("Cache-Control", cc)
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
	r   io.Reader
	n   int64
	err error // last non-EOF read error
}

// Read implements io.Reader.
func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF {
		c.err = err
	}
	return n, err
}

// Seek implements io.Seeker when the wrapped reader is one, so a countReader
// can be passed to http.ServeContent.
func (c *countReader) Seek(offset int64, whence int) (int64, error) {
	return c.r.(io.Seeker).Seek(offset, whence)
}

// serveFile streams f to the client with http.ServeContent; a mid-stream read
// error is logged and the connection is closed, so the client sees a truncated
// response instead of a silently short one.
func serveFile(w http.ResponseWriter, r *http.Request, name string, f io.ReadSeeker, modTime time.Time) {
	cr := &countReader{r: f}
	http.ServeContent(w, r, name, modTime, cr)
	if cr.err != nil {
		logger.Error("serving file failed", "path", r.URL.Path, cr.err)
		if conn, _, herr := http.NewResponseController(w).Hijack(); herr == nil {
			_ = conn.Close()
		}
	}
}

// NewRouter returns a new Router using the given operations.
func NewRouter(srv *Server, opts *RouterOptions) *Router {
	rt := &Router{
		opts: opts,
		srv:  srv,
	}
	if opts != nil {
		rt.pattern = opts.Pattern
		rt.downloadRoot = opts.DownloadRoot
		rt.cacheExpire = opts.CacheExpire
		rt.disableFetch = opts.DisableModuleFetch
		rt.tempDir = opts.TempDir
		if opts.Proxy == "" {
			logger.Info("not set proxy, all direct")
			return rt
		}
		steps, tail, err := parseProxyChain(opts.Proxy)
		if err != nil {
			logger.Error("parse proxy chain failed, all direct", "proxy", opts.Proxy, err)
			return rt
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if opts.Insecure {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		transport.MaxIdleConnsPerHost = 100
		transport.ResponseHeaderTimeout = 30 * time.Second
		if opts.DialTimeout > 0 {
			transport.DialContext = (&net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}).DialContext
		}
		rt.chainClient = &http.Client{Transport: transport}

		if len(steps) == 1 && tail == "" && (steps[0].u.Scheme == "http" || steps[0].u.Scheme == "https") {
			// Single upstream: stream through a reverse proxy (no buffering).
			remote := steps[0].u
			proxy := httputil.NewSingleHostReverseProxy(remote)
			director := proxy.Director               //nolint:staticcheck // Director is deprecated; Rewrite would drop the single-host joinURL rewrite
			proxy.Director = func(r *http.Request) { //nolint:staticcheck
				director(r)
				r.Host = remote.Host
			}
			proxy.ModifyResponse = rt.customModResponse
			proxy.Transport = transport
			// An unreachable upstream must not lose a cached copy.
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, perr error) {
				logger.Warn("proxy error", "url", r.URL, perr)
				if rt.serveStale(w, r) {
					return
				}
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, perr.Error(), http.StatusBadGateway)
			}
			rt.proxy = proxy
			return rt
		}

		if len(steps) == 0 {
			// Chain of only "direct"/"off": answer locally.
			logger.Warn("proxy chain has no upstream, all direct", "proxy", opts.Proxy)
			return rt
		}
		rt.chain = steps
		rt.tail = tail
		for i := range rt.chain {
			st := &rt.chain[i]
			if st.u.Scheme == "file" {
				// Local directory mirror: a per-step transport rooted at the
				// step's directory; http.Dir keeps served paths inside it.
				tr := transport.Clone()
				tr.RegisterProtocol("file", http.NewFileTransport(http.Dir(st.u.Path)))
				st.client = &http.Client{Transport: tr}
			}
		}
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

// ServeHTTP implements http handler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
	mw.Header().Add("Vary", HeaderDisableModuleFetch)

	if rt.fetchDisabled(r) {
		logger.Debug("cache-only", "url", r.URL)
		mw.Header().Set(HeaderDisableModuleFetch, "true")
		if _, served := rt.serveFromCache(mw, r, true); served {
			rt.count("cached", mw)
			return
		}
		http.Error(mw, ErrFetchDisabled.Error(), http.StatusGone)
		rt.count("cached", mw)
		return
	}

	if (rt.proxy == nil && rt.chain == nil) || rt.Direct(strings.TrimPrefix(r.URL.Path, "/")) {
		logger.Debug("direct", "url", r.URL)
		rt.srv.ServeHTTP(mw, r)
		rt.count("direct", mw)
		return
	}

	if _, served := rt.serveFromCache(mw, r, false); served {
		rt.count("cached", mw)
		return
	}
	if rt.proxy != nil {
		logger.Debug("proxy", "url", r.URL)
		rt.proxy.ServeHTTP(mw, r)
		rt.count("proxy", mw)
		return
	}
	logger.Debug("proxy-chain", "url", r.URL)
	rt.serveChain(mw, r)
	rt.count("proxy", mw)
}

// serveChain walks the GOPROXY-style -proxy chain after a cache miss.
// Not-exist answers (404/410) always fall through to the next step, other
// failures only after a "|", and the chain tail decides the ending: "direct"
// answers from the local go command, "off" (or an exhausted chain) serves a
// stale cached copy when one exists (stale-on-error), else the last upstream
// failure.
func (rt *Router) serveChain(mw *metricsResponseWriter, r *http.Request) {
	var lastStatus int
	var lastBody []byte
	for _, st := range rt.chain {
		status, tmp, cached, body, err := rt.fetchUpstream(st, r)
		switch {
		case err == nil && cached:
			if _, served := rt.serveFromCache(mw, r, false); served {
				return
			}
			lastStatus, lastBody = http.StatusBadGateway, nil
		case err == nil && status == http.StatusOK:
			// Cache-restricted 200: stream the fetched temp copy through,
			// without caching it, then drop the temp file.
			rt.serveTempFile(mw, r, tmp)
			return
		case err == nil && (status == http.StatusNotFound || status == http.StatusGone):
			lastStatus, lastBody = status, body
		default:
			if !st.fallBackOnError {
				rt.serveChainFailure(mw, r, status, body, err)
				return
			}
			lastStatus, lastBody = status, body
		}
	}
	if rt.tail == "direct" {
		if lastStatus == http.StatusNotFound || lastStatus == http.StatusGone {
			// An upstream authoritatively answered not-exist: a local go
			// command failure (missing VCS credentials, network) must not
			// upgrade it to a 500 that aborts the client's path walk-up.
			rt.srv.ServeHTTP(&notExistShield{ResponseWriter: mw, status: lastStatus, body: lastBody}, r)
			return
		}
		rt.srv.ServeHTTP(mw, r)
		return
	}
	rt.serveChainFailure(mw, r, lastStatus, lastBody, nil)
}

// notExistShield downgrades a local (go command) 5xx answer to the not-exist
// status an earlier chain step already gave for the same path.
type notExistShield struct {
	http.ResponseWriter
	status int
	body   []byte
	shadow bool
}

func (w *notExistShield) WriteHeader(code int) {
	if code >= 500 && !w.shadow {
		w.shadow = true
		http.Error(w.ResponseWriter, string(w.body), w.status)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *notExistShield) Write(p []byte) (int, error) {
	if w.shadow {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *notExistShield) ReadFrom(r io.Reader) (int64, error) {
	if w.shadow {
		return 0, nil
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *notExistShield) Flush() {
	if !w.shadow {
		w.ResponseWriter.(http.Flusher).Flush()
	}
}

func (w *notExistShield) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// fetchUpstream fetches r's path from one chain step. A 200 body is streamed
// into the download cache (cached=true) — or into a temporary file, returned
// as tmp, when the upstream marks the response as uncacheable. Non-200
// answers return the status and a capped copy of the body. Transport failures
// and retryable upstream statuses (429/5xx) are retried twice with a small
// linear backoff, honoring the upstream's Retry-After when it is short.
func (rt *Router) fetchUpstream(st upstreamStep, r *http.Request) (status int, tmp string, cached bool, body []byte, err error) {
	u := *st.u
	if st.u.Scheme == "file" {
		// The per-step transport serves file requests rooted at the step's
		// directory (http.Dir), so the URL carries the request path as-is.
		u.Host = "mirror"
		u.Path = r.URL.Path
	} else {
		u.Path = path.Join(u.Path, r.URL.Path)
	}
	for attempt := 0; ; attempt++ {
		var retryAfter time.Duration
		status, tmp, cached, body, retryAfter, err = rt.fetchUpstreamOnce(st, r, u.String())
		if (err == nil && !retryableStatus(status)) || attempt >= 2 || permanentTransportError(err) {
			return
		}
		delay := max(retryAfter, time.Duration(attempt+1)*100*time.Millisecond)
		if delay > 2*time.Second {
			// Do not stretch the retry window for a long Retry-After: fail
			// now and let the chain/tail decide.
			return
		}
		time.Sleep(delay)
	}
}

// retryableStatus reports whether a chain step's HTTP failure is worth
// retrying (rate limit or transient server/gateway failure). Like the go
// command's GOPROXY handling, not-exist answers (404/410) never are.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// permanentTransportError reports a transport failure that will not heal on
// retry: the host does not resolve, its certificate is rejected, or the URL
// scheme is unsupported.
func permanentTransportError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}
	var uaErr x509.UnknownAuthorityError
	var hvErr x509.HostnameError
	var ciErr x509.CertificateInvalidError
	if errors.As(err, &uaErr) || errors.As(err, &hvErr) || errors.As(err, &ciErr) {
		return true
	}
	return strings.Contains(err.Error(), "unsupported protocol scheme")
}

// retryAfterDelay returns the delay requested by the Retry-After header
// (delta-seconds or HTTP date), 0 when absent or unparsable.
func retryAfterDelay(h http.Header) time.Duration {
	v := strings.TrimSpace(strings.Join(h.Values("Retry-After"), ","))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseUint(v, 10, 64); err == nil {
		const maxSecs = uint64(math.MaxInt64 / int64(time.Second))
		if secs > maxSecs {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (rt *Router) fetchUpstreamOnce(st upstreamStep, r *http.Request, urlStr string) (status int, tmp string, cached bool, body []byte, retryAfter time.Duration, rerr error) {
	client := st.client
	if client == nil {
		client = rt.chainClient
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, urlStr, nil)
	if err != nil {
		return 0, "", false, nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", false, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, "", false, body, retryAfterDelay(resp.Header), nil
	}
	if CacheRestricted(resp.Header) {
		f, err := os.CreateTemp(rt.tempDir, "goproxy.upstream.*")
		if err != nil {
			return 0, "", false, nil, 0, err
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = os.Remove(f.Name())
			return 0, "", false, nil, 0, err
		}
		return http.StatusOK, f.Name(), false, nil, 0, nil
	}
	file := cacheFileFor(rt.downloadRoot, r.URL.Path)
	if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
		return 0, "", false, nil, 0, err
	}
	if err := renameio.WriteToFile(file, resp.Body, 0o666); err != nil {
		return 0, "", false, nil, 0, err
	}
	return http.StatusOK, "", true, nil, 0, nil
}

// serveChainFailure answers an exhausted chain: a cached copy wins over the
// upstream failure (stale-on-error), else the last upstream status is passed
// through; a chain that only saw transport failures answers 502.
func (rt *Router) serveChainFailure(mw *metricsResponseWriter, r *http.Request, status int, body []byte, err error) {
	if _, served := rt.serveFromCache(mw, r, true); served {
		return
	}
	if status == 0 {
		msg := "all upstream proxies failed"
		if err != nil {
			msg = err.Error()
		}
		mw.Header().Set("Cache-Control", "no-store")
		http.Error(mw, msg, http.StatusBadGateway)
		return
	}
	mw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	mw.WriteHeader(status)
	_, _ = mw.Write(body)
}

// serveStale serves r from the download cache, ignoring all cache expiry,
// reporting whether a response was written.
func (rt *Router) serveStale(w http.ResponseWriter, r *http.Request) bool {
	if !isModuleFile(r.URL.Path) {
		return false
	}
	f, err := os.Open(cacheFileFor(rt.downloadRoot, r.URL.Path))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	if ctype := contentTypeFor(r.URL.Path); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	if cc := cacheControlFor(r.URL.Path); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	serveFile(w, r, "", f, info.ModTime())
	return true
}

// serveTempFile streams a temporary file (a cache-restricted upstream copy)
// to the client with the module file's content type, then removes it.
func (rt *Router) serveTempFile(mw *metricsResponseWriter, r *http.Request, file string) {
	defer func() { _ = os.Remove(file) }()
	f, err := os.Open(file)
	if err != nil {
		mw.Header().Set("Cache-Control", "no-store")
		http.Error(mw, "upstream response not cacheable", http.StatusBadGateway)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		mw.Header().Set("Cache-Control", "no-store")
		http.Error(mw, "upstream response not cacheable", http.StatusBadGateway)
		return
	}
	if ctype := contentTypeFor(r.URL.Path); ctype != "" {
		mw.Header().Set("Content-Type", ctype)
	}
	serveFile(mw, r, "", f, info.ModTime())
}

// contentTypeFor returns the content type for a module file served from
// the download cache, or "" if the path does not name a module file.
func contentTypeFor(p string) string {
	if strings.HasSuffix(p, "/@latest") {
		return "text/plain; charset=UTF-8"
	}
	_, after, ok := strings.Cut(p, "/@v/")
	if !ok {
		return ""
	}
	what := after
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
	file := cacheFileFor(rt.downloadRoot, r.URL.Path)
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
	what := ""
	if i := strings.Index(r.URL.Path, "/@v/"); i >= 0 {
		what = r.URL.Path[i+len("/@v/"):]
	}
	if !bypassTTL {
		if strings.HasSuffix(r.URL.Path, "/@latest") && time.Since(info.ModTime()) >= ListExpire {
			return true, false
		}
		if what == "list" && time.Since(info.ModTime()) >= rt.cacheExpire {
			return true, false
		}
	}
	if what == "list" && info.Size() == 0 {
		// Negative cache marker (the path was resolved as not-a-module within
		// the cache TTL): answer 404 from cache, without upstream or go command.
		http.Error(mw, "not found", http.StatusNotFound)
		return true, true
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
	serveFile(mw, r, "", f, info.ModTime())
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
