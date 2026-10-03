# goproxy — Command-Line Reference

`goproxy` is a Go module proxy. It does **not** fetch modules itself: for each
request it shells out to the local `go` command (`go mod download -json`,
`go list -m -versions`) and serves the resulting files from the Go module
download cache. A working `go` toolchain is a hard runtime requirement.

```
goproxy [flags]
```

Every flag below is declared in `main.go` (`setup()`). Defaults shown are the
runtime defaults.

## Flag summary

| Flag | Type | Default | Purpose |
|------|------|---------|---------|
| `-listen` | string | `0.0.0.0:8081` | Service listen address |
| `-cacheDir` | string | *(empty)* | Go module cache dir (default `$GOPATH/pkg/mod/cache/download`) |
| `-proxy` | string | *(empty)* | Upstream proxy; a GOPROXY-style chain (see below) |
| `-exclude` | string | *(empty)* | Comma-separated glob patterns routed direct (sets `GOPRIVATE`) |
| `-cacheExpire` | duration | `5m` | TTL for cached `list` files and `@latest` |
| `-disableModuleFetch` | bool | `false` | Serve from cache only; never fetch upstream |
| `-gcInterval` | duration | `0` (off) | Cache GC interval |
| `-gcKeep` | duration | `336h` (14d) | Age (since last access) at which GC deletes a cache file |
| `-sumdb` | string | *(empty)* | Extra proxied checksum databases |
| `-sumdbProxy` | string | *(empty)* | Route all sumdb requests through this host |
| `-tlsCert` | string | *(empty)* | TLS certificate file (with `-tlsKey` → HTTPS) |
| `-tlsKey` | string | *(empty)* | TLS key file (with `-tlsCert` → HTTPS) |
| `-pathPrefix` | string | *(empty)* | Prefix stripped from all request paths |
| `-goBin` | string | `go` | Path to the go binary used for direct fetches |
| `-maxConcurrentFetches` | int | `0` (= 2×NumCPU) | Cap on concurrent go subprocesses |
| `-connectTimeout` | duration | `30s` | Dial timeout for upstream (proxy/sumdb) connections |
| `-fetchTimeout` | duration | `10m` | Max time a single request may take (`0` = no limit) |
| `-tempDir` | string | *(empty)* = `$TMPDIR` | Dir for upstream stream-through temp files |
| `-version` | bool | `false` | Print the build version and exit |

---

## Serving

### `-listen`
Address the HTTP server binds to. Default `0.0.0.0:8081`.

### `-pathPrefix`
Prefix stripped from every request path before routing, so the whole service
(module protocol, `/metrics`, `/healthz`) can be mounted under a sub-path,
e.g. `-pathPrefix /goproxy` → requests to `/goproxy/github.com/x/y/@v/list`.

### `-tlsCert`, `-tlsKey`
When **both** are set the server serves HTTPS via `ListenAndServeTLS`;
otherwise plain HTTP. Setting only one has no TLS effect.

### `-version`
Prints the version embedded at build time (`-ldflags "-X main.version=..."`)
and exits. `make` embeds `git describe --tags --always --dirty`.

### Endpoints (not flags)
- `GET /metrics` — Prometheus metrics (served by the access-log handler).
- `GET /healthz` — `204 No Content` liveness probe, `Cache-Control: no-store`,
  served outside the counted module handlers.

---

## Caching

### `-cacheDir`
Root of the Go module download cache. When set, goproxy sets
`GOMODCACHE=<cacheDir>/pkg/mod` and serves from
`<cacheDir>/pkg/mod/cache/download`. When empty it asks the go command for
`$GOPATH` and uses `$GOPATH/pkg/mod/cache/download`.

> **Do not share this cache with the proxy's own clients.** A client locks a
> module as "being downloaded" before its request reaches the proxy, which then
> waits on that same download → deadlock. Use a separate `-cacheDir`
> (the e2e test uses `GOPATH=/tmp/go`).

### `-cacheExpire`
TTL applied to cached `list` files and `@latest` lookups. Default `5m`.
Immutable files (`.info`/`.mod`/`.zip`, sumdb `tile/`+`lookup/`) are not
governed by this — they are served long-term and expire only via GC.

### `-disableModuleFetch`
Global cache-only mode (per-request equivalent: the `Disable-Module-Fetch: true`
request header). When active, every handler serves from the download cache
only — list/`@latest` are served even past their TTL, and no go command or
upstream is ever invoked. A cache miss returns `410 Gone` with a
`Disable-Module-Fetch: true` response header.

### `-gcInterval`, `-gcKeep`
Periodic atime-based garbage collection of the download cache.
- `-gcInterval 0` (default) disables GC entirely.
- Every interval, files whose **last access time** is older than `-gcKeep`
  (default `336h`, i.e. 14 days) are deleted, then directories that became
  empty are pruned deepest-first. The cache root and non-empty dirs are never
  removed.
- Only the download cache is scanned (`…/cache/download`); the git/VCS cache
  under `…/cache/vcs` is untouched.
- Safe against a running proxy: freshly fetched files have a recent atime.
- atime access is build-tagged per platform (`gc_unix.go` / `gc_darwin.go` /
  `gc_windows.go`); platforms without atime are a no-op.

---

## Routing / upstream

### `-proxy`
The upstream proxy. Enables **router mode**. Accepts either a single URL or a
full GOPROXY-style **chain**; the semantics mirror the go command's own
`GOPROXY` list handling.

- **Single URL** (the common case) — a streaming `httputil.ReverseProxy` to
  that upstream; 200 responses are streamed into the local download cache and
  later served from cache.
- **Chain** — comma/pipe-separated entries tried in order:
  - After a **`,`** the next entry is tried **only** when the current one
    answers "not found" (404/410).
  - After a **`|`** the next entry is tried on **any** failure (connection
    error, 5xx).
  - A **`file:///path/to/dir`** entry is a local directory mirror laid out in
    the download-cache format (served offline, no network).
  - The chain may end with **`direct`** (fall back to the local go command)
    and/or **`off`** (stop; answer from the cache or with the last upstream
    failure). These must be the last entry.
  - Entries answering `429`/`5xx` are retried up to twice (100/200 ms linear
    backoff, short `Retry-After` honored); a `Retry-After` longer than 2s
    abandons the step.

```
goproxy -proxy "https://a.example,https://b.example|direct,off"
```

**Stale-on-error:** when an upstream fetch fails, a previously cached copy of
the module file is served if one exists. Upstream responses marked
`no-store`/`no-cache`/`must-revalidate`/`private`/`proxy-revalidate`/
`s-maxage`/`max-age=0`/`Vary: *` are passed through **without** being cached.

### `-exclude`
Comma-separated glob patterns (`path.Match` semantics) that should **not** go
through the `-proxy` upstream. Two effects:
1. Sets `GOPRIVATE=<exclude>` so the underlying go command treats those paths
   as private.
2. In router mode, a module whose full path matches a glob bypasses the
   upstream and is resolved **directly** by the local go command.

Patterns are matched against the **full module path**, not just the host
component.

```
goproxy -proxy https://goproxy.io -exclude "github.com/my-org/*,rsc.io/private"
```

### `-goBin`
The go binary invoked for direct (non-upstream) fetches. Default `go`.

### `-maxConcurrentFetches`
Cap on concurrent go command subprocesses, so a cold-cache request storm can't
spawn hundreds of simultaneous clones. Default `0` = `2 × runtime.NumCPU()`.

### `-connectTimeout`
Dial timeout applied to all upstream (proxy/sumdb) transports. Default `30s`.

### `-fetchTimeout`
Maximum wall-clock time a single request may take, enforced via a per-request
context timeout. Default `10m`; `0` disables the limit.

### `-tempDir`
Directory for stream-through upstream temp files. Default `$TMPDIR`.

---

## Checksum database (sumdb)

By default goproxy proxies the built-in sum databases — `sum.golang.org`,
`sum.golang.google.cn`, `gosum.io` — to their upstream mirrors in priority
order (first 200 wins). Content-addressed requests (`tile/...`, `lookup/...`)
are cached under the cache dir in the go client's own layout and served from
cache later; `latest` is never cached.

### `-sumdb`
Register **additional** (e.g. private) checksum databases:
`"name url,name2 url2"` — the URL is optional and defaults to `https://name`.
Requests to `/sumdb/<name>/...` are then served from that URL and cached like
the built-ins. Multi-segment names work (e.g. `corp.example.com/sumdb`).

```
goproxy -proxy https://goproxy.io -sumdb "sumdb.corp.example.com,https://sumdb.corp.example.com/"
```

### `-sumdbProxy`
Rewrite **all** sumdb hosts (including the built-ins) to route through this
proxy host instead of the direct mirrors. Empty (default) = direct mirrors.

```
goproxy -proxy https://goproxy.io -sumdbProxy https://goproxy.cn
```

---

## Environment invariants

`setup()` (in `main.go`) unconditionally sets these so the underlying `go`
command always fetches directly and never loops back into the proxy:
`GO111MODULE=on`, `GOPROXY=direct`, `GOSUMDB=off`, `GIT_TERMINAL_PROMPT=0`,
and `GOPRIVATE` (from `-exclude`). Do not remove these.

## Examples

```shell
# Plain proxy, isolated cache
goproxy -listen 0.0.0.0:8081 -cacheDir /var/lib/goproxy

# Router: public via goproxy.io, private org resolved direct
goproxy -listen 0.0.0.0:8081 -proxy https://goproxy.io -exclude "github.com/my-org/*"

# HTTPS + sub-path + GC
goproxy -listen 0.0.0.0:443 -tlsCert /etc/ssl/goproxy.crt -tlsKey /etc/ssl/goproxy.key \
        -pathPrefix /goproxy -gcInterval 1h -gcKeep 720h

# Cache-only (never fetch)
goproxy -listen 127.0.0.1:8081 -disableModuleFetch
```
