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
	"net/http"
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
	"golang.org/x/mod/sumdb/tlog"
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

	// sumdbHostTimeout bounds each single upstream attempt, body read
	// included (tiles are a few KB over TLS).
	sumdbHostTimeout = 10 * time.Second
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	whichDB, rawPath, err := parsePath(r.URL.Path)
	if _, supported := supportedSumDB[whichDB]; err != nil || !supported {
		w.WriteHeader(http.StatusGone)
		_, _ = fmt.Fprint(w, "unsupported db")
		return
	}

	// Rebuild the on-disk/upstream path from the validated db name and a
	// rooted-cleaned path, so ".." segments cannot escape the cache dir.
	p := strings.TrimPrefix(path.Clean("/"+rawPath), "/")

	// $GOROOT/src/cmd/go/internal/modfetch/sumdb.go@initBase
	// > Before accessing any checksum database URL using a proxy, the proxy
	// > client should first fetch <proxyURL>/sumdb/<sumdb-name>/supported.
	if p == "supported" {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Reject any path that is not a valid sumdb protocol path before it can
	// reach the cache dir or an upstream.
	if err := validatePath(p); err != nil {
		http.Error(w, errSumPathInvalid.Error(), http.StatusNotFound)
		return
	}

	// Tiles and lookups are content-addressed (immutable) and cacheable;
	// latest is volatile and is never cached.
	cacheable := strings.HasPrefix(p, "tile/") || strings.HasPrefix(p, "lookup/")
	if cacheable && h.serveCache(w, r, whichDB, p) {
		return
	}
	if fetchDisabled(r, h.fetchDisabled) {
		w.Header().Set("Disable-Module-Fetch", "true")
		http.Error(w, "module fetch is disabled", http.StatusGone)
		return
	}

	var (
		firstSeen   bool
		firstStatus int
		firstData   []byte
	)
	for _, host := range supportedSumDB[whichDB] {
		status, data, err := h.fetch(r.Context(), host, p)
		if err != nil {
			logger.Error("sumdb: proxy request failed", "host", host, "path", p, err)
			continue
		}
		// An empty 200 body is a broken mirror, not a valid record:
		// fall through to the next host.
		if status == http.StatusOK && len(data) > 0 {
			if cacheable && h.downloadRoot != "" {
				h.writeCache(whichDB, p, data)
			}
			respondSumdb(w, http.StatusOK, p, data)
			return
		}
		if !firstSeen {
			firstSeen, firstStatus, firstData = true, status, data
		}
	}
	if firstSeen {
		if firstStatus == http.StatusOK {
			http.Error(w, "empty sumdb response", http.StatusBadGateway)
			return
		}
		respondSumdb(w, firstStatus, p, firstData)
		return
	}
	http.Error(w, "all sumdb upstreams failed", http.StatusGone)
}

// fetchDisabled reports whether this request must be answered from the cache
// only (global flag or Disable-Module-Fetch request header).
func fetchDisabled(r *http.Request, global bool) bool {
	v, _ := strconv.ParseBool(r.Header.Get("Disable-Module-Fetch"))
	return global || v
}

// validatePath checks that a cleaned sumdb sub-path is one of the protocol's
// paths, and that tile and lookup paths are well formed (valid, canonical
// module coordinates; bounded tile level), so garbage never reaches the cache
// dir and never hits the mirrors.
func validatePath(p string) error {
	switch {
	case p == "latest":
		return nil
	case strings.HasPrefix(p, "lookup/"):
		escPath, escVersion, ok := strings.Cut(strings.TrimPrefix(p, "lookup/"), "@")
		if !ok {
			return errSumPathInvalid
		}
		mp, err := module.UnescapePath(escPath)
		if err != nil {
			return err
		}
		vers, err := module.UnescapeVersion(escVersion)
		if err != nil {
			return err
		}
		if err := module.Check(mp, vers); err != nil {
			return err
		}
		if vers != module.CanonicalVersion(vers) {
			return errSumPathInvalid
		}
		return nil
	case strings.HasPrefix(p, "tile/"):
		// maxTileLevel is the maximum level documented by tlog.Tile,
		// which tlog.ParseTilePath does not enforce.
		const maxTileLevel = 63
		tile, err := tlog.ParseTilePath(p)
		if err != nil {
			return err
		}
		if tile.L > maxTileLevel {
			return errSumPathInvalid
		}
		return nil
	default:
		return errSumPathInvalid
	}
}

// fetch fetches p from one sumdb host. The body is read here, under the
// per-host context, so it is complete before the context is cancelled.
func (h *Handler) fetch(ctx context.Context, host, p string) (int, []byte, error) {
	urlPath, err := url.Parse(host)
	if err != nil {
		return 0, nil, err
	}
	urlPath.Path = strings.TrimSuffix(urlPath.Path, "/") + "/" + p
	ctx, cancel := context.WithTimeout(ctx, sumdbHostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlPath.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	logger.Info("sumdb: proxy request", "url", urlPath.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
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
	if fetchDisabled(r, h.fetchDisabled) {
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
		logger.Error("sumdb: make cache dir failed", err)
		return
	}
	if err := renameio.WriteToFile(file, bytes.NewReader(data), 0666); err != nil {
		logger.Error("sumdb: write cache file failed", err)
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

// parsePath splits "/sumdb/<db>/<subpath>" into the proxied db name and the
// sub-path. Db names may span multiple segments ("corp.example.com/sumdb"),
// so the longest registered name that matches at a segment boundary wins.
func parsePath(rawPath string) (whichDB, path string, err error) {
	const dbPrefix = "/sumdb/"
	if !strings.HasPrefix(rawPath, dbPrefix) {
		return "", "", errSumPathInvalid
	}
	rest := rawPath[len(dbPrefix):]
	for name := range supportedSumDB {
		if len(name) <= len(whichDB) || !strings.HasPrefix(rest, name) {
			continue
		}
		if r := rest[len(name):]; r != "" && r[0] != '/' {
			continue
		}
		whichDB = name
	}
	if whichDB == "" {
		return "", "", errSumPathInvalid
	}
	return whichDB, strings.TrimPrefix(rest[len(whichDB):], "/"), nil
}

// AddProxiedDB registers (or replaces) a proxied checksum database: requests
// to /sumdb/<name>/... are served from the given upstream base url.
func AddProxiedDB(name, url string) {
	supportedSumDB[name] = []string{url}
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
