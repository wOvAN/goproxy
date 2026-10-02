// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRouterFetchDisabled(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "github.com", "x", "y", "@v", "list")
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("v1.0.0\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	// An expired list file: cache-only serving must bypass the TTL check.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}

	rt := NewRouter(NewServer(nil, nil), &RouterOptions{
		Proxy:        "http://127.0.0.1:1", // unreachable: any upstream hit fails
		DownloadRoot: root,
		CacheExpire:  time.Minute,
	})

	get := func(path string, cacheOnly bool) *http.Response {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://proxy/"+path, nil)
		if cacheOnly {
			req.Header.Set(HeaderDisableModuleFetch, "true")
		}
		rt.ServeHTTP(rec, req)
		return rec.Result()
	}

	// Cache-only hit: expired cache file is served, TTL bypassed, header echoed.
	resp := get("github.com/x/y/@v/list", true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cache-only hit status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get(HeaderDisableModuleFetch) != "true" {
		t.Error("cache-only response must echo Disable-Module-Fetch header")
	}
	resp.Body.Close()

	// Cache-only miss: 410, never proxied (the unreachable proxy would
	// surface as 502 if it were contacted).
	resp = get("github.com/x/miss/@v/list", true)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("cache-only miss status = %d, want 410", resp.StatusCode)
	}
	resp.Body.Close()

	// Normal mode with expired list and dead proxy: not 200 (falls to proxy).
	resp = get("github.com/x/y/@v/list", false)
	if resp.StatusCode == http.StatusOK {
		t.Error("expired cache with dead proxy must not serve 200 in normal mode")
	}
	resp.Body.Close()
}

func TestContentTypeFor(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/github.com/x/y/@latest", "text/plain; charset=UTF-8"},
		{"/github.com/x/y/@v/list", "text/plain; charset=UTF-8"},
		{"/github.com/x/y/@v/v1.0.0.info", "application/json"},
		{"/github.com/x/y/@v/v1.0.0.mod", "text/plain; charset=UTF-8"},
		{"/github.com/x/y/@v/v1.0.0.zip", "application/octet-stream"},
		{"/github.com/x/y", ""},
	}
	for _, tc := range tests {
		if got := contentTypeFor(tc.path); got != tc.want {
			t.Errorf("contentTypeFor(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestRouterTraversal pins down current behavior: the cache lookup joins
// the raw request path onto downloadRoot. url.URL.String/RequestURI of a
// cleaned path normally has no ".." segments, record the exact upstream
// path shape so a regression (or a fix) is visible.
func TestRouterCachePathJoin(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o666); err != nil {
		t.Fatal(err)
	}
	rt := NewRouter(NewServer(nil, nil), &RouterOptions{
		DownloadRoot: root,
	})
	rec := httptest.NewRecorder()
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/../" + filepath.Base(secret)},
		Header: http.Header{},
	}
	rt.serveFromCache(&metricsResponseWriter{ResponseWriter: rec}, req, true)
	// Must not read files outside downloadRoot.
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "s") {
		t.Errorf("path traversal: served %q from outside cache root", rec.Body.String())
	}
}
