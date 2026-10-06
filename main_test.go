package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goproxyio/goproxy/v2/proxy"
	"golang.org/x/mod/module"
)

func TestMapNotFound(t *testing.T) {
	notFound := fmt.Errorf("go: list -m -json -versions X@latest:\ngo: X@latest: no matching versions for query \"latest\"\n")
	if !errors.Is(mapNotFound(notFound), fs.ErrNotExist) {
		t.Error("go command not-found diagnostic must map to fs.ErrNotExist")
	}
	unrecognized := fmt.Errorf("go: list -m -json -versions X/y@latest:\ngo: X/y@latest: unrecognized import path \"X/y\": reading https://X/y?go-get=1: 404 Not Found\n")
	if !errors.Is(mapNotFound(unrecognized), fs.ErrNotExist) {
		t.Error("unrecognized import path must map to fs.ErrNotExist")
	}
	other := fmt.Errorf("go: mod download -json X@v1.0.0:\nexit status 1\n")
	if errors.Is(mapNotFound(other), fs.ErrNotExist) {
		t.Error("transient go command failure must not map to fs.ErrNotExist")
	}
	gitAuth := fmt.Errorf("go: list -m -json -versions github.com/a/b/c@latest:\ngo: github.com/a/b/c: git ls-remote -q origin: exit status 128:\nfatal: could not read Username for 'https://github.com': terminal prompts disabled\n")
	if errors.Is(mapNotFound(gitAuth), fs.ErrNotExist) {
		t.Error("git credential failure is a proxy-host problem, must stay 500")
	}
}

// TestFetchDisabled serves a primed download cache with
// Disable-Module-Fetch: true, without ever running the go command.
func TestFetchDisabled(t *testing.T) {
	downloadRoot = t.TempDir()
	const mod = "example.com/mod"
	escMod, _ := module.EscapePath(mod)
	dir := filepath.Join(downloadRoot, escMod, "@v")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "list"), []byte("v1.0.0\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	info := `{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "v1.0.0.info"), []byte(info), 0o666); err != nil {
		t.Fatal(err)
	}

	h := proxy.NewServer(new(ops), nil)
	get := func(path string, cacheOnly bool) *http.Response {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://proxy/"+path, nil)
		if cacheOnly {
			req.Header.Set(proxy.HeaderDisableModuleFetch, "true")
		}
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	// Cache-only hit: cached list and info are served with the header set.
	resp := get(mod+"/@v/list", true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cached list status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get(proxy.HeaderDisableModuleFetch) != "true" {
		t.Error("cache-only response must echo Disable-Module-Fetch header")
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.TrimSpace(string(body)) != "v1.0.0" {
		t.Errorf("cached list body = %q, want v1.0.0", body)
	}

	resp = get(mod+"/@latest", true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cached @latest status = %d, want 200", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "v1.0.0") {
		t.Errorf("@latest body = %q, want latest from cached list", body)
	}

	resp = get(mod+"/@v/v1.0.0.info", true)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cached info status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Cache-only miss: 410 with the header, no go command run.
	resp = get("example.com/other/@v/list", true)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("cache-only miss status = %d, want 410", resp.StatusCode)
	}
	if resp.Header.Get(proxy.HeaderDisableModuleFetch) != "true" {
		t.Error("cache-only miss must carry Disable-Module-Fetch header")
	}
	_ = resp.Body.Close()
}

func TestListNegativeCache(t *testing.T) {
	downloadRoot = t.TempDir()
	oldExpire := cacheExpire
	cacheExpire = time.Hour
	defer func() { cacheExpire = oldExpire }()
	const mpath = "example.com/a/b"
	file := listPath(mpath)
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := new(ops).List(context.Background(), mpath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fresh empty list file must serve as cached not-found, got %v", err)
	}
	if _, err := new(ops).Latest(context.Background(), mpath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fresh empty list file must answer @latest from cache, got %v", err)
	}
}
