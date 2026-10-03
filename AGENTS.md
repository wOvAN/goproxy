# AGENTS.md

goproxy is a Go module proxy. It does **not** fetch modules itself — it shells out to the local `go` command (`go mod download -json`, `go list -m -versions`) and serves files from the Go module download cache. A working `go` toolchain is a hard runtime requirement. Deep architecture notes (chain semantics, cache-control, GC, metrics, sumdb) live in `CLAUDE.md` — read it before touching `proxy/` or `sumdb/`.

## Commands

```shell
make            # go mod tidy + go build -> bin/goproxy (embeds version via `git describe`)
make test       # go test -v ./...
make lint       # golangci-lint run ./...   (no .golangci.yml — runs on defaults)
make image      # docker build -t goproxy/goproxy .
make clean      # git clean -f -d -X  — DESTRUCTIVE, removes untracked files incl. bin/
```

- Single test: `go test ./sumdb/ -run TestName`. Unit tests live in the root (`main_test.go`, `gc_test.go`) and in `proxy/`, `sumdb/`, `renameio/`.
- End-to-end (mirrors CI): start the proxy in the background, then `bash test/get_test.sh`.
  - Proxy mode: `bin/goproxy &`
  - Router mode: `bin/goproxy -proxy https://goproxy.io -exclude "golang.org" &`
  - The script `go get`s every module in `test/testdata/get.txt` with `GOPROXY=http://127.0.0.1:8081` and `GOPATH=/tmp/go`. **Requires a running proxy and network access.**

## Gotchas

- **Do not share a GOPATH with the proxy's own clients.** The client locks a module as "being downloaded" before its request reaches the proxy, which then waits on that same download → deadlock. Always use a separate `-cacheDir` (the e2e script uses `GOPATH=/tmp/go`).
- `setup()` in `main.go` forces `GOPROXY=direct`, `GOSUMDB=off`, `GO111MODULE=on` so the underlying `go` command never loops back into the proxy. Do not remove these.
- Flags are parsed in `setup()` (called from `main`), **not** `init()` — keep it that way so test binaries are not polluted with flag parsing.
- Use the vendored `renameio.WriteFile` (helpers in `renameio/` + `robustio/`) for cache-file writes, not raw `os.WriteFile` (atomic temp+rename, Windows retry, umask preservation).
- 404 vs 500 is decided by `errors.Is(err, fs.ErrNotExist)`; go-command not-found diagnostics are tagged in `mapNotFound` (`main.go`).

## Layout

- `main.go` — `ops` (shells to `go`), handler chain, flag/env setup, GC wiring.
- `proxy/server.go` — module proxy HTTP protocol (`/p/@v/list`, `@latest`, `@v/vN.info|.mod|.zip`).
- `proxy/router.go` + `proxy/chain.go` — router mode (`-proxy` GOPROXY-style chain, `-exclude` globs).
- `proxy/cachecontrol.go` — `Cache-Control` policy per path type.
- `sumdb/handler.go` — proxies `/sumdb/<db>/...` to mirrors, caches tiles/lookups.
- `gc.go` (+ `gc_unix.go`/`gc_darwin.go`/`gc_windows.go`) — atime-based cache GC (build-tagged per platform).

## CI

- CircleCI (`.circleci/config.yml`): `proxy-mode` and `router-mode` jobs — tidy → test → build → run proxy → `bash test/get_test.sh`. New code must pass unit tests **and** the live e2e.
- GitHub Actions (`.github/workflows/docker.yml`): builds/pushes a multi-arch GHCR image on tag push (`v*`).
- Go 1.27.
