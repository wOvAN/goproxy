# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

goproxy (goproxy.io) is a global proxy for Go modules. It does **not** fetch modules itself — it shells out to the local `go` command (`go mod download -json`, `go list -m -versions`) and serves the files from the Go module download cache. A working `go` toolchain is a runtime requirement.

## Commands

```shell
make            # build: go mod tidy + go build -o bin/goproxy -ldflags "-s -w"
make test       # go test -v ./...
make image      # docker build -t goproxy/goproxy .
make clean      # git clean -f -d -X — DESTRUCTIVE, removes untracked files incl. bin/
```

Run a single test: `go test ./renameio/ -run TestName` (unit tests exist only in `renameio/` and `sumdb/`).

End-to-end test (mirrors CI): start the proxy, then run the get script:

```shell
bin/goproxy &                          # or with router mode:
bin/goproxy -proxy https://goproxy.io -exclude "golang.org" &
bash test/get_test.sh                  # runs `go get` on every module in test/testdata/get.txt
```

`test/get_test.sh` sets `GOPROXY=http://127.0.0.1:8081` and `GOPATH=/tmp/go`.

## Architecture

Handler chain in `main.go`: `logger` (access log + `/metrics` via promhttp) wraps either `proxy.Server` (proxy mode) or `proxy.Router` (router mode, when `-proxy` is set).

- **`main.go` — `ops`** implements `proxy.ServerOps` by invoking the `go` command and serving files from the download cache root: `$cacheDir/pkg/mod/cache/download` (default `$GOPATH/pkg/mod/cache/download`). Layout: `<escaped-module-path>/@v/{list,.info,.mod,.zip}`. `list` files are cached with a TTL (`-cacheExpire`, default 5 min).
- **`proxy/server.go`** — the module proxy HTTP protocol: `/p/@v/list`, `/p/@latest`, `/p/@v/vN.info|.mod|.zip`. Unescapes module paths with `golang.org/x/mod/module`. 404 vs 500 is decided by `errors.Is(err, os.ErrNotFound)`.
- **`proxy/router.go`** — router mode: an `httputil.ReverseProxy` to the upstream (`-proxy`, e.g. https://goproxy.io). Module paths matching the `-exclude` comma-separated globs (`GlobsMatchPath`, matched against the full path, not just the host) bypass the upstream and go direct to the local `go` command. `ModifyResponse` (`customModResponse`) writes upstream responses atomically into the local download cache, so later requests are served from cache. `@latest` files expire on `ListExpire` (hardcoded 5 min); `list` files use `cacheExpire`. Note: the reverse proxy transport sets `InsecureSkipVerify: true`.
- **`sumdb/handler.go`** — no real sum database; `/sumdb/<db>/...` is proxied by racing `sum.golang.org` / `sum.golang.google.cn` / `gosum.io` with a 2s timeout and returning the first response. `/sumdb/<db>/supported` returns 200. Any other db name returns 410.
- **`renameio/` + `robustio/`** — vendored copies of the Go team's atomic-write helpers (temp file + rename, Windows retry logic, umask preservation). `router.go` uses `renameio.WriteFile` for cache writes. Keep using them for cache-file writes rather than raw `os.WriteFile`.

Environment invariants set in `main.go` `init()`: `GO111MODULE=on`, `GOPROXY=direct`, `GOSUMDB=off`, `GIT_TERMINAL_PROMPT=0`, and `GOPRIVATE` (from `-exclude`). These force the underlying `go` command to always fetch directly, so the proxy never loops back into itself.

**Deadlock constraint** (documented in `main.go`): the proxy must not share a GOPATH with its own clients — the client locks a module as "being downloaded" before sending the request to the proxy, which then waits on that same download. Use a separate `-cacheDir` (as `test/get_test.sh` does with `GOPATH=/tmp/go`).

## CI

CircleCI (`.circleci/config.yml`), two jobs — `proxy-mode` and `router-mode`. Each: `make tidy` → `make test` → `make build` → start `bin/goproxy` in the background → `bash test/get_test.sh`. New code must pass both unit tests and the live `go get` e2e.

## Docker

Image (`Dockerfile`) is multi-stage on `golang:alpine`, installs git/mercurial/subversion (needed for direct fetches), entrypoint is `tini` + `/goproxy`, default port 8081, volume `/go`.
