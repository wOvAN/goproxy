# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

goproxy (goproxy.io) is a global proxy for Go modules. It does **not** fetch modules itself — it shells out to the local `go` command (`go mod download -json`, `go list -m -versions`) and serves the files from the Go module download cache. A working `go` toolchain is a runtime requirement.

## Commands

```shell
make            # build: go mod tidy + go build -ldflags "-s -w -X main.version=$(git describe ...)" — embeds version, printed at startup and by `-version`
make test       # go test -v ./...
make lint       # golangci-lint run ./...
make image      # docker build -t goproxy/goproxy .
make clean      # git clean -f -d -X — DESTRUCTIVE, removes untracked files incl. bin/
```

Run a single test: `go test ./sumdb/ -run TestName` (unit tests in `main_test.go`, `renameio/`, `sumdb/`).

End-to-end test (mirrors CI): start the proxy, then run the get script:

```shell
bin/goproxy &                          # or with router mode:
bin/goproxy -proxy https://goproxy.io -exclude "golang.org" &
bash test/get_test.sh                  # runs `go get` on every module in test/testdata/get.txt
```

`test/get_test.sh` sets `GOPROXY=http://127.0.0.1:8081` and `GOPATH=/tmp/go`.

## Architecture

Handler chain in `main.go`: `requestLogger` (access log + `/metrics` via promhttp) wraps either `proxy.Server` (proxy mode) or `proxy.Router` (router mode, when `-proxy` is set).

- **`main.go` — `ops`** implements `proxy.ServerOps` by invoking the `go` command and serving files from the download cache root: `$cacheDir/pkg/mod/cache/download` (default `$GOPATH/pkg/mod/cache/download`). Layout: `<escaped-module-path>/@v/{list,.info,.mod,.zip}`. `list` files are cached with a TTL (`-cacheExpire`, default 5 min).
- **`proxy/server.go`** — the module proxy HTTP protocol: `/p/@v/list`, `/p/@latest`, `/p/@v/vN.info|.mod|.zip`. Unescapes module paths with `golang.org/x/mod/module`. 404 vs 500 is decided by `errors.Is(err, fs.ErrNotExist)` (go command not-found diagnostics are tagged in `mapNotFound`, main.go).
- **`proxy/router.go` + `proxy/chain.go`** — router mode. `-proxy` accepts a single URL (streaming `httputil.ReverseProxy` to the upstream) **or** a full GOPROXY-style chain (`proxy/chain.go: parseProxyChain`): `,` = fall through on not-exist (404/410) only, `|` = on any failure, trailing `direct` (local go command) / `off` (stop); chain mode fetches with its own `http.Client` (steps are retried on transport errors and on 429/500/502/503/504: 2 retries, 100/200ms linear backoff, a `Retry-After` longer than 2s abandons the step), streams 200s into the download cache, then `serveFromCache`. A `file:///dir` chain step is a local directory mirror: a per-step transport with `http.NewFileTransport(http.Dir(dir))` serves request paths rooted inside `dir` (no escape). Module paths matching the `-exclude` comma-separated globs (`GlobsMatchPath`, matched against the full path, not just the host) bypass the upstream and go direct to the local `go` command. `ModifyResponse` (`customModResponse`) streams upstream responses into the local download cache (`renameio.WriteToFile`, no full-response buffering), so later requests are served from cache; upstream 200s with cache-restricted `Cache-Control` (no-store/no-cache/must-revalidate/private/proxy-revalidate/s-maxage/`max-age=0`) or `Vary: *` are passed through uncached. **Stale-on-error**: a cached copy is served when the upstream fails — non-200 upstream (`customModResponse`), transport error (ReverseProxy `ErrorHandler`), chain exhaustion without `direct`, and in `ops` (list/@latest) when the go command fails. `@latest` files expire on `ListExpire` (hardcoded 5 min); `list` files use `cacheExpire`. Cache paths are built via `cacheFileFor` = `path.Clean("/"+url.Path)` under downloadRoot — `..` cannot escape it. Upstream TLS verification is on by default; `-insecure` sets `InsecureSkipVerify: true` on the router transport (and on the sumdb client via `sumdb.SetInsecure`).
- **`sumdb/handler.go`** — no real sum database; `sumdb.Handler` (built via `sumdb.NewHandler(downloadRoot, fetchDisabled)`, injected into `Server`/`Router`) proxies `/sumdb/<db>/...` to the db's upstream hosts **in priority order** (first 200 wins, 10s per host **including body read** — keep the ReadAll inside `fetch`, the per-host ctx dies at its return; all-fail → 410; empty 200 body = broken mirror, try next host). `validatePath` enforces protocol paths before cache/upstream: `latest`, `tile/...` parsed by `tlog.ParseTilePath` (canonical, `L <= 63`), `lookup/<path>@<version>` unescaped and `module.Check`ed + canonical-version checked; anything else 404. Content-addressed `tile/...` and `lookup/...` responses are cached immutably under `downloadRoot/sumdb/<db>/...` (the go client's own layout) and served from cache later; `latest` is never cached. `-sumdbProxy` (default empty = direct mirrors) rewrites the hosts to route through that proxy instead (`SetSumdbProxy`). `/sumdb/<db>/supported` returns 200. Any other db name returns 410. `sumdb.AddProxiedDB(name, url)` (flag `-sumdb "name url,..."`) registers additional proxied dbs; `parsePath` matches db names longest-first at segment boundaries, so multi-segment names (`corp.example.com/sumdb`) work, and `SetSumdbProxy` rewrites all registered names. Upstream paths are built from the cleaned path — `..` segments cannot escape the cache dir.
- **`renameio/` + `robustio/`** — vendored copies of the Go team's atomic-write helpers (temp file + rename, Windows retry logic, umask preservation). `router.go` uses `renameio.WriteFile` for cache writes. Keep using them for cache-file writes rather than raw `os.WriteFile`.

Environment invariants set in `main.go` `setup()` (flags parsed there, not `init()`, so test binaries are not polluted): `GO111MODULE=on`, `GOPROXY=direct`, `GOSUMDB=off`, `GIT_TERMINAL_PROMPT=0`, and `GOPRIVATE` (from `-exclude`). These force the underlying `go` command to always fetch directly, so the proxy never loops back into itself. Hot path: `doOnce` merges concurrent fetches of the same module into one go command run, `goCmdSem` bounds concurrent go subprocesses (2*NumCPU), and `cachedFile`/`List` serve from the download cache before ever spawning a go command.

**Cache-only mode (`Disable-Module-Fetch`)**: the request header `Disable-Module-Fetch: true` (or the global `-disableModuleFetch` flag, wired into `RouterOptions.DisableModuleFetch`, the `ops` ctx via `proxy.WithFetchDisabled`, and `sumdb.NewHandler`) makes every handler serve from the download cache only — list/@latest served past TTL, version files/sumdb tiles from cache, cache miss → `410` with a `Disable-Module-Fetch: true` response header, no go command and no upstream ever run. `proxy.ErrFetchDisabled` is the sentinel ops return for that mapping.

**Cache-Control (`proxy/cachecontrol.go: cacheControlFor`)**: list and `@latest` get `public, max-age=300`; `.info/.mod/.zip` and sumdb `tile/`+`lookup/` (immutable) get `max-age=604800`; sumdb `latest` 60; sumdb `supported` `no-store`. Applied in `Server.ServeHTTP`, `Router.serveFromCache`, `customModResponse` (upstream pass-through) and the sumdb handler.

**Metrics**: `proxy.MetricsMiddleware(mode, h)` counts proxy-mode (Server) requests under mode `direct`; router mode counts its own (`direct`/`proxy`/`cached`/`sumdb`) — do not wrap the router in the middleware, it would double-count.

**Protocol hygiene**: `Server.ServeHTTP` and `Router.ServeHTTP` answer `405` + `Allow: GET, HEAD` for any other method (the go command only uses GET/HEAD — a POST must not spawn a go subprocess), and module responses carry `Vary: Disable-Module-Fetch`. The cache-only header is parsed with `strconv.ParseBool` (accepts `1`/`t`/`True`), in router, sumdb and `ops.NewContext`.

**Cache GC (`gc.go`)**: `-gcInterval` (default 0 = off) runs a periodic `sweepCache(downloadRoot, gcKeep)` goroutine: deletes files in the download cache with atime older than `-gcKeep` (default 14 days), then prunes emptied dirs deepest-first; root itself and non-empty dirs are never removed. Portable atime access is per-platform (`gc_unix.go`/`gc_darwin.go`/`gc_windows.go` build-tagged on `atime(fs.FileInfo)`); platforms without atime are a no-op. GC scans only `downloadRoot` (not `pkg/mod/cache/vcs`), so git caches are untouched.

**Server options**: `-tlsCert`/`-tlsKey` (both set → `ListenAndServeTLS`), `-pathPrefix` (`http.StripPrefix` over the mux), `GET /healthz` → 204 `no-store` (served by the mux, outside the counted handlers), `-fetchTimeout` (per-request ctx timeout middleware), `-connectTimeout` → `RouterOptions.DialTimeout` (dialer timeout on all router upstream transports), `-tempDir` → `RouterOptions.TempDir` (chain stream-through temp files), `-goBin` (go binary for direct fetches), `-maxConcurrentFetches` (0 = default `2*NumCPU`), `-insecure` (skip upstream TLS verification, default false; also applied to the sumdb client).

**Deadlock constraint** (documented in `main.go`): the proxy must not share a GOPATH with its own clients — the client locks a module as "being downloaded" before sending the request to the proxy, which then waits on that same download. Use a separate `-cacheDir` (as `test/get_test.sh` does with `GOPATH=/tmp/go`).

## CI

CircleCI (`.circleci/config.yml`), two jobs — `proxy-mode` and `router-mode`. Each: `make tidy` → `make test` → `make build` → start `bin/goproxy` in the background → `bash test/get_test.sh`. New code must pass both unit tests and the live `go get` e2e.

## Docker

Image (`Dockerfile`) is multi-stage on `golang:alpine`, installs git/mercurial/subversion (needed for direct fetches), entrypoint is `tini` + `/goproxy`, default port 8081, volume `/go`.
