// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sumdb

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTestUpstreams points db at the given upstream base URLs for the
// duration of the test and restores the original map afterwards.
func useTestUpstreams(t *testing.T, db string, urls ...string) {
	t.Helper()
	orig := supportedSumDB[db]
	supportedSumDB[db] = urls
	t.Cleanup(func() { supportedSumDB[db] = orig })
}

func TestHandler(t *testing.T) {
	if ret := t.Run("supported", testSupported); !ret {
		t.Error("supported test failed, stop test")
		t.FailNow()
	}
	t.Run("fallback", testFallback)
	t.Run("cache", testCache)
	t.Run("fetchDisabled", testFetchDisabled)
	t.Run("validate", testValidate)
	t.Run("emptyUpstream", testEmptyUpstream)
}

func testSupported(t *testing.T) {
	type TestCase struct {
		name          string
		db            string
		wantSupported bool
	}

	tests := []TestCase{
		{
			name:          "sum.golang.org",
			db:            "sum.golang.org",
			wantSupported: true,
		},
		{
			name:          "gosum.io",
			db:            "gosum.io",
			wantSupported: true,
		},
		{
			name:          "sum.golang.google.cn",
			db:            "sum.golang.google.cn",
			wantSupported: true,
		},
		{
			name:          "other",
			db:            "other",
			wantSupported: false,
		},
	}

	h := NewHandler(t.TempDir(), false)
	for _, testcase := range tests {
		t.Run(testcase.name, func(t *testing.T) {
			recoder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("https://goproxy.io/sumdb/%s/supported", testcase.db), nil)
			h.ServeHTTP(recoder, req)

			resp := recoder.Result()
			if support := (resp.StatusCode == http.StatusOK); support != testcase.wantSupported {
				t.Errorf("db %s: want %v got %v", testcase.db, testcase.wantSupported, support)
			}
			_ = resp.Body.Close()
		})
	}
}

func testFallback(t *testing.T) {
	var hits1, hits2 int
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits1++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits2++
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer s2.Close()

	useTestUpstreams(t, "sum.golang.org", s1.URL, s2.URL)
	h := NewHandler(t.TempDir(), false)

	// Primary fails → fall back to secondary.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/lookup/github.com/x/y@v1.0.0", nil)
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("fallback status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if hits1 != 1 || hits2 != 1 {
		t.Errorf("fallback hits = (%d, %d), want (1, 1)", hits1, hits2)
	}

	// Healthy primary short-circuits the secondary.
	base2 := hits2
	s2b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("secondary must not be contacted when primary answers 200")
	}))
	defer s2b.Close()
	useTestUpstreams(t, "sum.golang.org", s2.URL, s2b.URL)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/latest", nil)
	h.ServeHTTP(rec, req)
	resp = rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("primary status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// s2 (index 0) answered once more; s2b (index 1) must not have been
	// hit at all (it would t.Error).
	if hits2 != base2+1 {
		t.Errorf("short-circuit: healthy primary hits = %d, want %d", hits2, base2+1)
	}

	// All upstreams unreachable → 410.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	useTestUpstreams(t, "sum.golang.org", deadURL)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/latest", nil)
	h.ServeHTTP(rec, req)
	resp = rec.Result()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("all-fail status = %d, want 410", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func testCache(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = fmt.Fprint(w, "tile-data")
	}))
	defer up.Close()

	root := t.TempDir()
	useTestUpstreams(t, "sum.golang.org", up.URL)
	h := NewHandler(root, false)

	get := func(path string) *http.Response {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://goproxy.io"+path, nil)
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	// First lookup fetches upstream and writes the cache file.
	resp := get("/sumdb/sum.golang.org/lookup/github.com/x/y@v1.0.0")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first lookup status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if hits != 1 {
		t.Fatalf("upstream hits after first lookup = %d, want 1", hits)
	}
	cacheFile := filepath.Join(root, "sumdb", "sum.golang.org", "lookup", "github.com/x/y@v1.0.0")
	data, err := os.ReadFile(filepath.FromSlash(cacheFile))
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	if string(data) != "tile-data" {
		t.Errorf("cache content = %q, want %q", data, "tile-data")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
		t.Errorf("immutable lookup Cache-Control = %q, want public max-age", cc)
	}

	// Second lookup is served from cache without touching upstream.
	resp = get("/sumdb/sum.golang.org/lookup/github.com/x/y@v1.0.0")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached lookup status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if hits != 1 {
		t.Errorf("upstream hits after cached lookup = %d, want still 1", hits)
	}

	// @latest is volatile: never cached, proxied every time.
	resp = get("/sumdb/sum.golang.org/latest")
	_ = resp.Body.Close()
	if hits != 2 {
		t.Errorf("upstream hits after latest = %d, want 2 (latest must not be cached)", hits)
	}

	// Strict path validation: traversal segments clean to "etc/passwd",
	// which is not a sumdb protocol path → 404, upstream untouched.
	hitsBefore := hits
	resp = get("/sumdb/sum.golang.org/lookup/../../../../etc/passwd")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("traversal status = %d, want 404", resp.StatusCode)
	}
	if hits != hitsBefore {
		t.Errorf("invalid sumdb path hit upstream %d times, want 0", hits-hitsBefore)
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); err == nil {
		t.Error("traversal wrote outside the sumdb cache dir")
	}
}

func testFetchDisabled(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = fmt.Fprint(w, "d")
	}))
	defer up.Close()

	root := t.TempDir()
	useTestUpstreams(t, "sum.golang.org", up.URL)
	h := NewHandler(root, false)

	// Prime the cache with one lookup.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/lookup/github.com/x/y@v1.0.0", nil)
	h.ServeHTTP(rec, req)
	_ = rec.Result().Body.Close()
	hits = 0

	// A handler in cache-only mode serves the cached entry and 410s misses,
	// never touching upstream.
	hd := NewHandler(root, true)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/lookup/github.com/x/y@v1.0.0", nil)
	hd.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cache-only hit status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Disable-Module-Fetch"); got != "true" {
		t.Errorf("cache-only hit Disable-Module-Fetch = %q, want true", got)
	}
	_ = resp.Body.Close()

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/latest", nil)
	hd.ServeHTTP(rec, req)
	resp = rec.Result()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("cache-only miss status = %d, want 410", resp.StatusCode)
	}
	if got := resp.Header.Get("Disable-Module-Fetch"); got != "true" {
		t.Errorf("cache-only miss Disable-Module-Fetch = %q, want true", got)
	}
	_ = resp.Body.Close()

	if hits != 0 {
		t.Errorf("cache-only mode touched upstream %d times, want 0", hits)
	}
}

// testValidate pins strict sumdb path validation: malformed tile/lookup
// paths never reach the cache dir or an upstream.
func testValidate(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = fmt.Fprint(w, "data")
	}))
	defer up.Close()

	root := t.TempDir()
	useTestUpstreams(t, "sum.golang.org", up.URL)
	h := NewHandler(root, false)

	get := func(path string) *http.Response {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://goproxy.io"+path, nil)
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	for _, path := range []string{
		"/sumdb/sum.golang.org/lookup/github.com/x/y",    // no @version
		"/sumdb/sum.golang.org/lookup/github.com/x/y@v1", // non-canonical version
		"/sumdb/sum.golang.org/lookup/github.com/x/y@latest",
		"/sumdb/sum.golang.org/tile/garbage",
		"/sumdb/sum.golang.org/tile/0/0/000",  // height 0 not a valid tile
		"/sumdb/sum.golang.org/tile/1/64/000", // tile level too deep
		"/sumdb/sum.golang.org/nonsense",
	} {
		resp := get(path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if hits != 0 {
		t.Errorf("invalid paths hit upstream %d times, want 0", hits)
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		t.Errorf("invalid paths wrote into cache dir: %v", entries)
	}

	// Well-formed paths pass validation and reach upstream.
	resp := get("/sumdb/sum.golang.org/tile/1/0/000")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits != 1 {
		t.Errorf("valid tile: status = %d, hits = %d, want 200/1", resp.StatusCode, hits)
	}
}

// testEmptyUpstream: an empty 200 body is a broken mirror — the handler must
// fall through to the next host, and 502 when all mirrors are empty.
func testEmptyUpstream(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer empty.Close()
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "record")
	}))
	defer full.Close()

	useTestUpstreams(t, "sum.golang.org", empty.URL, full.URL)
	h := NewHandler(t.TempDir(), false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://goproxy.io/sumdb/sum.golang.org/lookup/github.com/x/y@v1.0.0", nil)
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("empty-first fallback status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// All mirrors empty: 502. A fresh handler (unprimed cache) must fetch.
	useTestUpstreams(t, "sum.golang.org", empty.URL)
	h2 := NewHandler(t.TempDir(), false)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	resp = rec.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("all-empty status = %d, want 502", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestParsePath(t *testing.T) {
	type TestCase struct {
		name        string
		rawPath     string
		wantWhichDB string
		wantPath    string
		wantErr     bool
	}

	tests := []TestCase{
		{
			name:        "valid path with sum.golang.org",
			rawPath:     "/sumdb/sum.golang.org/supported",
			wantWhichDB: "sum.golang.org",
			wantPath:    "supported",
			wantErr:     false,
		},
		{
			name:        "valid path with lookup",
			rawPath:     "/sumdb/sum.golang.org/lookup/github.com/test@v1.0.0",
			wantWhichDB: "sum.golang.org",
			wantPath:    "lookup/github.com/test@v1.0.0",
			wantErr:     false,
		},
		{
			name:        "valid path with gosum.io",
			rawPath:     "/sumdb/gosum.io/supported",
			wantWhichDB: "gosum.io",
			wantPath:    "supported",
			wantErr:     false,
		},
		{
			name:    "invalid path - too few parts",
			rawPath: "/sumdb/sum.golang.org",
			wantErr: true,
		},
		{
			name:    "invalid path - empty",
			rawPath: "",
			wantErr: true,
		},
		{
			name:    "invalid path - only root",
			rawPath: "/",
			wantErr: true,
		},
		{
			name:    "invalid path - only sumdb",
			rawPath: "/sumdb",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			whichDB, p, err := parsePath(tc.rawPath)

			if tc.wantErr {
				if err == nil {
					t.Errorf("parsePath(%q) expected error, got nil", tc.rawPath)
				}
				if err != errSumPathInvalid {
					t.Errorf("parsePath(%q) expected errSumPathInvalid, got %v", tc.rawPath, err)
				}
				return
			}

			if err != nil {
				t.Errorf("parsePath(%q) unexpected error: %v", tc.rawPath, err)
				return
			}

			if whichDB != tc.wantWhichDB {
				t.Errorf("parsePath(%q) whichDB = %q, want %q", tc.rawPath, whichDB, tc.wantWhichDB)
			}

			if p != tc.wantPath {
				t.Errorf("parsePath(%q) path = %q, want %q", tc.rawPath, p, tc.wantPath)
			}
		})
	}
}

func TestSetSumdbProxy(t *testing.T) {
	originalSupportedSumDB := make(map[string][]string)
	for k, v := range supportedSumDB {
		originalSupportedSumDB[k] = append([]string(nil), v...)
	}
	defer func() {
		supportedSumDB = originalSupportedSumDB
	}()

	type TestCase struct {
		name      string
		proxyHost string
		wantURLs  map[string]string
	}

	tests := []TestCase{
		{
			name:      "empty proxy host - should not modify",
			proxyHost: "",
			wantURLs: map[string]string{
				"sum.golang.org":       "https://sum.golang.org/",
				"sum.golang.google.cn": "https://sum.golang.google.cn/",
				"gosum.io":             "https://gosum.io/",
			},
		},
		{
			name:      "set proxy host without trailing slash",
			proxyHost: "https://goproxy.cn",
			wantURLs: map[string]string{
				"sum.golang.org":       "https://goproxy.cn/sumdb/sum.golang.org/",
				"sum.golang.google.cn": "https://goproxy.cn/sumdb/sum.golang.google.cn/",
				"gosum.io":             "https://goproxy.cn/sumdb/gosum.io/",
			},
		},
		{
			name:      "set proxy host with trailing slash",
			proxyHost: "https://goproxy.cn/",
			wantURLs: map[string]string{
				"sum.golang.org":       "https://goproxy.cn/sumdb/sum.golang.org/",
				"sum.golang.google.cn": "https://goproxy.cn/sumdb/sum.golang.google.cn/",
				"gosum.io":             "https://goproxy.cn/sumdb/gosum.io/",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			supportedSumDB = make(map[string][]string)
			for k, v := range originalSupportedSumDB {
				supportedSumDB[k] = append([]string(nil), v...)
			}

			SetSumdbProxy(tc.proxyHost)

			if tc.proxyHost == "" {
				for dbName := range supportedSumDB {
					if len(supportedSumDB[dbName]) != len(originalSupportedSumDB[dbName]) {
						t.Errorf("SetSumdbProxy(\"\") modified %s unexpectedly", dbName)
					}
				}
				return
			}

			for dbName, expectedURL := range tc.wantURLs {
				urls, exists := supportedSumDB[dbName]
				if !exists {
					t.Errorf("SetSumdbProxy() removed db %s", dbName)
					continue
				}
				if len(urls) != 1 {
					t.Errorf("SetSumdbProxy() db %s has %d URLs, want 1", dbName, len(urls))
					continue
				}
				if urls[0] != expectedURL {
					t.Errorf("SetSumdbProxy() db %s = %q, want %q", dbName, urls[0], expectedURL)
				}
			}
		})
	}
}

func TestHandlerInvalidPath(t *testing.T) {
	type TestCase struct {
		name           string
		path           string
		expectedStatus int
	}

	tests := []TestCase{
		{
			name:           "path too short",
			path:           "/sumdb/sum.golang.org",
			expectedStatus: http.StatusGone,
		},
		{
			name:           "unsupported database",
			path:           "/sumdb/unsupported.db.com/supported",
			expectedStatus: http.StatusGone,
		},
	}

	h := NewHandler(t.TempDir(), false)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("https://goproxy.io%s", tc.path), nil)
			h.ServeHTTP(recorder, req)

			resp := recorder.Result()
			if resp.StatusCode != tc.expectedStatus {
				t.Errorf("Handler() status = %d, want %d", resp.StatusCode, tc.expectedStatus)
			}
			_ = resp.Body.Close()
		})
	}
}
