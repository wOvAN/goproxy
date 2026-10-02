// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sumdb implements sumdb handler proxy.
package sumdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/goproxyio/goproxy/v2/renameio"
)

// supportedSumDB maps a sumdb name to its upstream hosts, in priority
// order: hosts are tried in order and the first 200 response wins.
var supportedSumDB = map[string][]string{
	"sum.golang.org":       {"https://sum.golang.org/", "https://sum.golang.google.cn/"},
	"sum.golang.google.cn": {"https://sum.golang.google.cn/", "https://sum.golang.org/"}, // db-name `sum.golang.google.cn` will be replaced in go
	"gosum.io":             {"https://gosum.io/"},
}

var (
	errSumPathInvalid = errors.New("sumdb request path invalid")

	// sumdbHostTimeout bounds each single upstream attempt.
	sumdbHostTimeout = 2 * time.Second
)

// A Handler serves /sumdb/<db>/... requests by proxying the supported
// checksum databases. Content-addressed paths (tile/... and lookup/...)
// are immutable, so they are cached under downloadRoot/sumdb/<db>/... —
// the same layout the go command uses for its own sumdb cache — and
// served from there on later requests.
//
// When fetchDisabled is set (or a request carries the
// Disable-Module-Fetch: true header), only the cache is served; a cache
// miss answers 410.
type Handler struct {
	downloadRoot  string
	fetchDisabled bool
}

// NewHandler returns a sumdb handler caching under downloadRoot.
// An empty downloadRoot disables caching.
func NewHandler(downloadRoot string, fetchDisabled bool) *Handler {
	return &Handler{downloadRoot: downloadRoot, fetchDisabled: fetchDisabled}
}

// ServeHTTP implements http handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	whichDB, rawPath, err := parsePath(r.URL.Path)
	if _, supported := supportedSumDB[whichDB]; err != nil || !supported {
		w.WriteHeader(http.StatusGone)
		_, _ = fmt.Fprint(w, "unsupported db")
		return
	}

	// Rebuild the on-disk/upstream path from the validated db name and a
	// rooted-cleaned path, so ".." segments cannot escape the cache dir.
	p := strings.TrimPrefix(path.Clean("/"+rawPath), "/")
	if p == "" || p == "." {
		http.Error(w, errSumPathInvalid.Error(), http.StatusBadRequest)
		return
	}

	// $GOROOT/src/cmd/go/internal/modfetch/sumdb.go@initBase
	// > Before accessing any checksum database URL using a proxy, the proxy
	// > client should first fetch <proxyURL>/sumdb/<sumdb-name>/supported.
	if p == "supported" {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Tiles and lookups are content-addressed (immutable) and cacheable;
	// latest is volatile and is never cached.
	cacheable := strings.HasPrefix(p, "tile/") || strings.HasPrefix(p, "lookup/")
	if cacheable && h.serveCache(w, r, whichDB, p) {
		return
	}
	if h.fetchDisabled || r.Header.Get("Disable-Module-Fetch") == "true" {
		w.Header().Set("Disable-Module-Fetch", "true")
		http.Error(w, "module fetch is disabled", http.StatusGone)
		return
	}

	var first *http.Response
	for _, host := range supportedSumDB[whichDB] {
		resp, err := h.fetch(r.Context(), host, p)
		if err != nil {
			log.Printf("[sumdb] proxy request to %s%s failed: %v\n", host, p, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			defer func() { _ = resp.Body.Close() }()
			data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if err == nil {
				if cacheable && h.downloadRoot != "" {
					h.writeCache(whichDB, p, data)
				}
				respondSumdb(w, http.StatusOK, p, data)
				return
			}
			resp.Body.Close()
			first = resp
			continue
		}
		if first == nil {
			first = resp
		} else {
			_ = resp.Body.Close()
		}
	}
	if first != nil {
		defer func() { _ = first.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(first.Body, 1<<20))
		respondSumdb(w, first.StatusCode, p, data)
		return
	}
	http.Error(w, "all sumdb upstreams failed", http.StatusGone)
}

func (h *Handler) fetch(ctx context.Context, host, p string) (*http.Response, error) {
	urlPath, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	urlPath.Path = strings.TrimSuffix(urlPath.Path, "/") + "/" + p
	ctx, cancel := context.WithTimeout(ctx, sumdbHostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlPath.String(), nil)
	if err != nil {
		return nil, err
	}
	log.Printf("[sumdb] proxy request to: %s\n", urlPath.String())
	return http.DefaultClient.Do(req)
}

// cachePath returns the download-cache file for a sumdb path.
func (h *Handler) cachePath(db, p string) string {
	return filepath.Join(h.downloadRoot, "sumdb", db, filepath.FromSlash(p))
}

// serveCache serves an immutable sumdb entry from the download cache,
// reporting whether a response was written.
func (h *Handler) serveCache(w http.ResponseWriter, r *http.Request, db, p string) bool {
	if h.downloadRoot == "" {
		return false
	}
	f, err := os.Open(h.cachePath(db, p))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if h.fetchDisabled || r.Header.Get("Disable-Module-Fetch") == "true" {
		w.Header().Set("Disable-Module-Fetch", "true")
	}
	w.Header().Set("Cache-Control", "public, max-age=604800")
	http.ServeContent(w, r, p, info.ModTime(), f)
	return true
}

// writeCache stores an immutable sumdb response in the download cache.
func (h *Handler) writeCache(db, p string, data []byte) {
	file := h.cachePath(db, p)
	if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
		log.Printf("[sumdb] make cache dir failed: %v\n", err)
		return
	}
	if err := renameio.WriteToFile(file, bytes.NewReader(data), 0666); err != nil {
		log.Printf("[sumdb] write cache file failed: %v\n", err)
	}
}

func respondSumdb(w http.ResponseWriter, status int, p string, data []byte) {
	switch {
	case strings.HasPrefix(p, "tile/"), strings.HasPrefix(p, "lookup/"):
		w.Header().Set("Cache-Control", "public, max-age=604800")
	case p == "latest":
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func parsePath(rawPath string) (whichDB, path string, err error) {
	parts := strings.SplitN(rawPath, "/", 4)
	if len(parts) < 4 {
		return "", "", errSumPathInvalid
	}
	whichDB = parts[2]
	path = parts[3]
	return
}

// SetSumdbProxy rewrites the supported sumdb hosts to route through proxyHost.
func SetSumdbProxy(proxyHost string) {
	if proxyHost == "" {
		return
	}
	proxyHost = strings.TrimSuffix(proxyHost, "/")
	for dbName := range supportedSumDB {
		proxyURL := proxyHost + "/sumdb/" + dbName + "/"
		supportedSumDB[dbName] = []string{proxyURL}
	}
}
