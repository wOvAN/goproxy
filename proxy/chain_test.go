// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proxy

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
)

func readAll(rc io.ReadCloser) (string, error) {
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func TestParseProxyChain(t *testing.T) {
	tests := []struct {
		in       string
		wantURLs []string
		wantPipe []bool
		wantTail string
		wantErr  bool
	}{
		{in: "https://a.example", wantURLs: []string{"https://a.example"}, wantPipe: []bool{false}},
		{in: "https://a.example,https://b.example|direct", wantURLs: []string{"https://a.example", "https://b.example"}, wantPipe: []bool{false, true}, wantTail: "direct"},
		{in: "https://a.example,off", wantURLs: []string{"https://a.example"}, wantPipe: []bool{false}, wantTail: "off"},
		{in: " https://a.example ,, https://b.example ", wantURLs: []string{"https://a.example", "https://b.example"}, wantPipe: []bool{false, false}},
		{in: "direct", wantTail: "direct"}, // tail-only chain: all direct
		{in: "off", wantTail: "off"},
		{in: "file:///srv/mirror", wantURLs: []string{"file:///srv/mirror"}, wantPipe: []bool{false}},
		{in: "file://", wantErr: true},                                    // file needs a path
		{in: "ftp://a.example", wantErr: true},                            // unsupported scheme
		{in: "https://a.example,direct,https://b.example", wantErr: true}, // direct not last
		{in: "a.example", wantErr: true},                                  // scheme-less
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		steps, tail, err := parseProxyChain(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseProxyChain(%q): want error, got steps=%v tail=%q", tc.in, steps, tail)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProxyChain(%q): %v", tc.in, err)
			continue
		}
		if tail != tc.wantTail {
			t.Errorf("parseProxyChain(%q) tail = %q, want %q", tc.in, tail, tc.wantTail)
		}
		if len(steps) != len(tc.wantURLs) {
			t.Errorf("parseProxyChain(%q) len = %d, want %d", tc.in, len(steps), len(tc.wantURLs))
			continue
		}
		for i, st := range steps {
			if st.u.String() != tc.wantURLs[i] {
				t.Errorf("parseProxyChain(%q)[%d] = %q, want %q", tc.in, i, st.u, tc.wantURLs[i])
			}
			if st.fallBackOnError != tc.wantPipe[i] {
				t.Errorf("parseProxyChain(%q)[%d] fallBackOnError = %v, want %v", tc.in, i, st.fallBackOnError, tc.wantPipe[i])
			}
		}
	}
}

// stubOps answers from memory so router tests can exercise the direct tail
// without a go command.
type stubOps struct {
	listBody string
	// directErr, when set, is returned by every fetch method: a generic
	// local failure (Server maps it to 500).
	directErr error
}

func (s *stubOps) NewContext(r *http.Request) (context.Context, error) { return r.Context(), nil }
func (s *stubOps) List(context.Context, string) (File, error) {
	if s.directErr != nil {
		return nil, s.directErr
	}
	return MemFile([]byte(s.listBody), time.Now()), nil
}
func (s *stubOps) Latest(context.Context, string) (File, error) {
	if s.directErr != nil {
		return nil, s.directErr
	}
	return nil, fs.ErrNotExist
}
func (s *stubOps) Info(context.Context, module.Version) (File, error) {
	if s.directErr != nil {
		return nil, s.directErr
	}
	return nil, fs.ErrNotExist
}
func (s *stubOps) GoMod(context.Context, module.Version) (File, error) {
	if s.directErr != nil {
		return nil, s.directErr
	}
	return nil, fs.ErrNotExist
}
func (s *stubOps) Zip(context.Context, module.Version) (File, error) {
	if s.directErr != nil {
		return nil, s.directErr
	}
	return nil, fs.ErrNotExist
}

func chainUpstream(status int, body string, header http.Header, hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestRouterChain(t *testing.T) {
	root := t.TempDir()

	var hits1, hits2 int
	u1 := chainUpstream(http.StatusNotFound, "missing", nil, &hits1)
	defer u1.Close()
	u2 := chainUpstream(http.StatusOK, "v1.0.0\n", nil, &hits2)
	defer u2.Close()

	newRT := func(proxy string) *Router {
		return NewRouter(NewServer(&stubOps{listBody: "v9.9.9\n"}, nil), &RouterOptions{
			Proxy:        proxy,
			DownloadRoot: root,
			CacheExpire:  time.Minute,
		})
	}
	get := func(rt *Router, path string, header http.Header) (int, string, http.Header) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://proxy/"+path, nil)
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		rt.ServeHTTP(rec, req)
		resp := rec.Result()
		body, _ := readAll(resp.Body)
		return resp.StatusCode, body, resp.Header
	}

	// Comma chain: not-exist on u1 falls through to u2; the 200 is cached.
	rt := newRT(u1.URL + "," + u2.URL)
	status, body, hdr := get(rt, "github.com/x/y/@v/list", nil)
	if status != http.StatusOK || body != "v1.0.0\n" {
		t.Errorf("chain fallback: status = %d, body = %q, want 200/v1.0.0", status, body)
	}
	if hdr.Get("Vary") != HeaderDisableModuleFetch {
		t.Errorf("module response Vary = %q, want %q", hdr.Get("Vary"), HeaderDisableModuleFetch)
	}
	if hits1 != 1 || hits2 != 1 {
		t.Errorf("chain hits = (%d, %d), want (1, 1)", hits1, hits2)
	}
	if data, err := os.ReadFile(filepath.Join(root, "github.com", "x", "y", "@v", "list")); err != nil || string(data) != "v1.0.0\n" {
		t.Errorf("chain success must cache the module file: data=%q err=%v", data, err)
	}

	// Comma chain: an internal (5xx) upstream failure does NOT fall through.
	dead := chainUpstream(http.StatusInternalServerError, "boom", nil, nil)
	defer dead.Close()
	rt = NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{Proxy: dead.URL + "," + u2.URL, DownloadRoot: t.TempDir(), CacheExpire: time.Minute})
	hits2 = 0
	status, _, _ = get(rt, "github.com/x/err/@v/list", nil)
	if status != http.StatusInternalServerError {
		t.Errorf("comma internal failure: status = %d, want passthrough 500", status)
	}
	if hits2 != 0 {
		t.Errorf("comma internal failure fell through: hits = %d, want 0", hits2)
	}

	// Pipe chain: the same 5xx falls through to the healthy upstream.
	rt = NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{Proxy: dead.URL + "|" + u2.URL, DownloadRoot: t.TempDir(), CacheExpire: time.Minute})
	status, body, _ = get(rt, "github.com/x/err/@v/list", nil)
	if status != http.StatusOK || body != "v1.0.0\n" {
		t.Errorf("pipe fallback: status = %d, body = %q, want 200/v1.0.0", status, body)
	}

	// Tail "direct": exhausted chain answers from the local ops.
	// (fresh module path: x/y is already cached in root from the first case)
	rt = newRT(u1.URL + ",direct")
	hits1 = 0
	status, body, _ = get(rt, "github.com/x/direct/@v/list", nil)
	if status != http.StatusOK || body != "v9.9.9\n" {
		t.Errorf("tail direct: status = %d, body = %q, want 200/v9.9.9", status, body)
	}

	// Tail "off" with a not-exist chain: the upstream 404 is passed through.
	rt = newRT(u1.URL + ",off")
	status, _, _ = get(rt, "github.com/x/none/@v/list", nil)
	if status != http.StatusNotFound {
		t.Errorf("tail off miss: status = %d, want 404", status)
	}

	// Upstream not-exist + local fetch failure: the upstream 404 is
	// authoritative, the local 500 must not override it.
	rt = NewRouter(NewServer(&stubOps{directErr: errors.New("git ls-remote: exit status 128")}, nil), &RouterOptions{
		Proxy:        u1.URL + ",direct",
		DownloadRoot: t.TempDir(),
		CacheExpire:  time.Minute,
	})
	status, _, _ = get(rt, "github.com/x/shielded/@v/list", nil)
	if status != http.StatusNotFound {
		t.Errorf("upstream 404 + local failure: status = %d, want 404", status)
	}

	// Stale-on-error: chain exhausted, cached (expired) copy wins.
	rt = newRT(u1.URL + ",off")
	status, body, _ = get(rt, "github.com/x/y/@v/list", nil)
	if status != http.StatusOK || body != "v1.0.0\n" {
		t.Errorf("chain stale-on-error: status = %d, body = %q, want 200/v1.0.0", status, body)
	}

	// Disable-Module-Fetch accepts ParseBool spellings.
	rt = newRT(u2.URL)
	hits2 = 0
	status, _, hdr = get(rt, "github.com/x/y/@v/list", http.Header{HeaderDisableModuleFetch: []string{"1"}})
	if status != http.StatusOK || hdr.Get(HeaderDisableModuleFetch) != "true" {
		t.Errorf("header '1': status = %d, resp header = %q, want 200/true", status, hdr.Get(HeaderDisableModuleFetch))
	}
	if hits2 != 0 {
		t.Error("cache-only request must not touch upstream")
	}
}

func TestRouterChainAllDead(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
		Proxy:        deadURL, // single dead upstream: reverse-proxy path
		DownloadRoot: t.TempDir(),
		CacheExpire:  time.Minute,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://proxy/github.com/x/z/@v/list", nil)
	rt.ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusBadGateway {
		t.Errorf("single dead upstream status = %d, want 502", rec.Result().StatusCode)
	}
}

func TestRouterCacheRestrictedNotCached(t *testing.T) {
	restricted := []http.Header{
		{"Cache-Control": []string{"no-store"}},
		{"Cache-Control": []string{"no-cache, max-age=60"}},
		{"Cache-Control": []string{"public, max-age=0"}},
		{"Cache-Control": []string{"s-maxage=60"}},
		{"Cache-Control": []string{"proxy-revalidate"}},
		{"Vary": []string{"*"}},
	}
	for i, hdr := range restricted {
		root := t.TempDir()
		var hits int
		up := chainUpstream(http.StatusOK, "v1.0.0\n", hdr, &hits)
		rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
			Proxy:        up.URL,
			DownloadRoot: root,
			CacheExpire:  time.Minute,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://proxy/github.com/x/y/@v/list", nil)
		rt.ServeHTTP(rec, req)
		resp := rec.Result()
		body, _ := readAll(resp.Body)
		if resp.StatusCode != http.StatusOK || body != "v1.0.0\n" {
			t.Errorf("restricted[%d] %v passthrough: status = %d, body = %q", i, hdr, resp.StatusCode, body)
		}
		if _, err := os.Stat(filepath.Join(root, "github.com", "x", "y", "@v", "list")); !os.IsNotExist(err) {
			t.Errorf("restricted[%d] %v: upstream response must not be written to the cache", i, hdr)
		}
		up.Close()
	}

	// Control: a plain cacheable 200 IS cached.
	root := t.TempDir()
	up := chainUpstream(http.StatusOK, "v1.0.0\n", http.Header{"Cache-Control": []string{"public, max-age=60"}}, nil)
	defer up.Close()
	rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{Proxy: up.URL, DownloadRoot: root, CacheExpire: time.Minute})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://proxy/github.com/x/y/@v/list", nil)
	rt.ServeHTTP(rec, req)
	resp := rec.Result()
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cacheable status = %d, want 200", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "github.com", "x", "y", "@v", "list")); err != nil {
		t.Errorf("cacheable 200 must be written to the cache: %v", err)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{DownloadRoot: t.TempDir()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy/github.com/x/y/@v/list", nil)
	rt.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("router POST status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("router POST Allow = %q, want GET listed", allow)
	}
	_ = resp.Body.Close()

	srv := NewServer(&stubOps{}, nil)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://proxy/github.com/x/y/@v/list", nil)
	srv.ServeHTTP(rec, req)
	resp = rec.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("server POST status = %d, want 405", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestRouterChainRetryAfter covers chain-step status retries: a short
// Retry-After is honored and the step is retried; a Retry-After beyond the
// retry window abandons the step immediately.
func TestRouterChainRetryAfter(t *testing.T) {
	var shortHits int
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shortHits++
		if shortHits == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("v1.0.0\n"))
	}))
	defer flaky.Close()

	rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
		Proxy:        flaky.URL + ",off",
		DownloadRoot: t.TempDir(),
		CacheExpire:  time.Minute,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://proxy/github.com/x/ra/@v/list", nil)
	rt.ServeHTTP(rec, req)
	resp := rec.Result()
	body, _ := readAll(resp.Body)
	if resp.StatusCode != http.StatusOK || body != "v1.0.0\n" {
		t.Errorf("429 retry: status = %d, body = %q, want 200/v1.0.0", resp.StatusCode, body)
	}
	if shortHits != 2 {
		t.Errorf("429 retry: hits = %d, want 2", shortHits)
	}

	var longHits int
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		longHits++
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer long.Close()
	var healthyHits int
	healthy := chainUpstream(http.StatusOK, "v2.0.0\n", nil, &healthyHits)
	defer healthy.Close()

	rt = NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
		Proxy:        long.URL + "|" + healthy.URL,
		DownloadRoot: t.TempDir(),
		CacheExpire:  time.Minute,
	})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://proxy/github.com/x/ra2/@v/list", nil)
	rt.ServeHTTP(rec, req)
	resp = rec.Result()
	body, _ = readAll(resp.Body)
	if resp.StatusCode != http.StatusOK || body != "v2.0.0\n" {
		t.Errorf("long Retry-After: status = %d, body = %q, want 200/v2.0.0", resp.StatusCode, body)
	}
	if longHits != 1 {
		t.Errorf("long Retry-After: first-step hits = %d, want 1 (no retry)", longHits)
	}
}

// TestRouterChainFile covers a file:// chain step: a local directory in the
// download-cache layout serves requests (and its responses get cached), with
// the served paths contained inside the step's directory.
func TestRouterChainFile(t *testing.T) {
	seed := t.TempDir()
	file := filepath.Join(seed, "github.com", "x", "f", "@v", "list")
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("v3.0.0\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o666); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
		Proxy:        "file://" + seed + ",off",
		DownloadRoot: root,
	})
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://proxy/"+path, nil)
		rt.ServeHTTP(rec, req)
		resp := rec.Result()
		body, _ := readAll(resp.Body)
		return resp.StatusCode, body
	}

	if status, body := get("github.com/x/f/@v/list"); status != http.StatusOK || body != "v3.0.0\n" {
		t.Errorf("file step: status = %d, body = %q, want 200/v3.0.0", status, body)
	}
	if data, err := os.ReadFile(filepath.Join(root, "github.com", "x", "f", "@v", "list")); err != nil || string(data) != "v3.0.0\n" {
		t.Errorf("file step must cache the module file: data=%q err=%v", data, err)
	}
	// Traversal must stay inside the seed dir.
	if status, body := get("../" + filepath.Base(secret)); status == http.StatusOK && strings.Contains(body, "s") {
		t.Errorf("file step traversal: served %q from outside the seed dir", body)
	}
}

// TestRouterInsecure pins the -insecure semantics: upstream TLS certificates
// are verified by default (an untrusted chain fails the fetch), and
// RouterOptions.Insecure skips verification.
func TestRouterInsecure(t *testing.T) {
	tlsUpstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("v1.0.0\n"))
	}))
	defer tlsUpstream.Close()

	get := func(rt *Router, path string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://proxy/"+path, nil)
		rt.ServeHTTP(rec, req)
		resp := rec.Result()
		body, _ := readAll(resp.Body)
		return resp.StatusCode, body
	}

	rt := NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
		Proxy:        tlsUpstream.URL,
		DownloadRoot: t.TempDir(),
	})
	if status, _ := get(rt, "github.com/x/tls/@v/list"); status != http.StatusBadGateway {
		t.Errorf("verified TLS upstream: status = %d, want 502 (untrusted certificate must fail)", status)
	}

	rt = NewRouter(NewServer(&stubOps{}, nil), &RouterOptions{
		Proxy:        tlsUpstream.URL,
		DownloadRoot: t.TempDir(),
		Insecure:     true,
	})
	if status, body := get(rt, "github.com/x/tls/@v/list"); status != http.StatusOK || body != "v1.0.0\n" {
		t.Errorf("Insecure upstream: status = %d, body = %q, want 200/v1.0.0", status, body)
	}
}
