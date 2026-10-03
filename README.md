# GOPROXY

[![CircleCI](https://circleci.com/gh/goproxyio/goproxy.svg?style=svg)](https://circleci.com/gh/goproxyio/goproxy)
[![Go Report Card](https://goreportcard.com/badge/github.com/goproxyio/goproxy)](https://goreportcard.com/report/github.com/goproxyio/goproxy)
[![GoDoc](https://godoc.org/github.com/goproxyio/goproxy?status.svg)](https://godoc.org/github.com/goproxyio/goproxy)

A global proxy for go modules. see: [https://goproxy.io](https://goproxy.io)

## Requirements

This service invokes the local `go` command to answer requests.

The default `cacheDir` is `GOPATH`, you can set it up by yourself according to the situation.

## Build

```shell
git clone https://github.com/goproxyio/goproxy.git
cd goproxy
make
```

## Started

### Proxy mode    

```shell
./bin/goproxy -listen=0.0.0.0:80 -cacheDir=/tmp/test
```

If you run `go get -v pkg` in the proxy machine, you should set a new `GOPATH` which is different from the original `GOPATH`, or you may encounter a deadlock.

See [`test/get_test.sh`](./test/get_test.sh).

### Router mode    

```shell
./bin/goproxy -listen=0.0.0.0:80 -proxy https://goproxy.io
```

Use the `-proxy` flag combined with the `-exclude` flag to enable `Router mode`, which implements route filter to routing private modules or public modules.

```
                                         direct
                      +----------------------------------> private repo
                      |
                 match|pattern
                      |
                  +---+---+           +----------+
go get  +-------> |goproxy| +-------> |goproxy.io| +---> golang.org/x/net
                  +-------+           +----------+
                 router mode           proxy mode
```

In `Router mode`, use the `-exclude` flag to set a glob pattern. The glob will specify what packages should not try to resolve with the value of `-proxy`. Modules which match the `-exclude` pattern will resolve direct to the repo which 
matches the module path.

NOTE: Patterns are matched to the full path specified, not only to the host component.

```shell
./bin/goproxy -listen=0.0.0.0:80 -cacheDir=/tmp/test -proxy https://goproxy.io -exclude "*.corp.example.com,rsc.io/private"
```

The `-proxy` flag accepts a full `GOPROXY`-style chain, not just one URL:

```shell
./bin/goproxy -proxy "https://a.example,https://b.example|direct,off"
```

Entries are tried in order. After a `,` the next entry is tried only when the current one answers "not found" (404/410); after a `|` it is tried on any failure (connection error, 5xx). The chain may end with `direct` (fall back to the local `go` command) or `off` (stop; answer from the cache or with the last upstream failure), as the single last entry. A single URL (the common case) keeps the streaming reverse-proxy behavior. A chain entry may also be `file:///path/to/dir` — a local directory in the download-cache layout, served as an offline mirror. Chain entries that answer `429`/`5xx` are retried up to twice (short `Retry-After` delays are honored).

When an upstream fetch fails, a previously cached copy of the module file is served when one exists (stale-on-error); upstream responses marked `no-store`/`no-cache`/`must-revalidate`/`private`/`proxy-revalidate`/`s-maxage`/`max-age=0`/`Vary: *` are passed through without being cached.

### Custom checksum databases

By default the built-in sum databases (`sum.golang.org`, `sum.golang.google.cn`, `gosum.io`) are proxied. To proxy additional (e.g. private) checksum databases, use the `-sumdb` flag:

```shell
./bin/goproxy -proxy https://goproxy.io -sumdb "sumdb.corp.example.com https://sumdb.corp.example.com/"
```

Each entry is `name` or `name url` (space-separated; url defaults to `https://name`), entries are comma-separated. Requests to `/sumdb/<name>/...` are served from that url and cached like the built-in databases.

### TLS, health check, other flags

Serve HTTPS by passing a certificate and key:

```shell
./bin/goproxy -listen=0.0.0.0:443 -tlsCert=/etc/ssl/goproxy.crt -tlsKey=/etc/ssl/goproxy.key
```

`GET /healthz` answers `204 No Content` (liveness probes). With `-pathPrefix /prefix`, all routes (module protocol, `/metrics`, `/healthz`) are served under that prefix.

Other flags:

- `-goBin` (default `go`) — the go binary used for direct fetches.
- `-maxConcurrentFetches` (default 0 = 2*NumCPU) — cap on concurrent direct fetches.
- `-connectTimeout` (default 30s) — dial timeout for upstream connections.
- `-fetchTimeout` (default 10m, 0 = unlimited) — maximum time a single request may take.
- `-tempDir` — directory for stream-through upstream temp files (default `$TMPDIR`).
- `-insecure` (default `false`) — skip upstream TLS certificate verification (router transports and the sumdb client). Prefer keeping it off; it was the previous hardcoded behavior.

### SumDB Proxy

By default, sumdb (Checksum Database) requests are sent directly to the built-in mirrors of the requested database (`sum.golang.org`, `sum.golang.google.cn`, `gosum.io`). Mirrors are tried in order and the first successful response wins; content-addressed requests (`tile/...`, `lookup/...`) are cached under the cache dir and served from there later. You can route all sumdb requests through a specific host using the `-sumdbProxy` flag:

```shell
./bin/goproxy -listen=0.0.0.0:80 -proxy https://goproxy.io -sumdbProxy https://goproxy.cn
```

When the `-sumdbProxy` flag is set, all sumdb requests (including `sum.golang.org`, `sum.golang.google.cn`, and `gosum.io`) will be proxied through the specified host.

### Cache-only mode (Disable-Module-Fetch)

To serve requests strictly from the local cache, without ever fetching upstream, send the request header `Disable-Module-Fetch: true`:

```shell
curl -H 'Disable-Module-Fetch: true' http://127.0.0.1:8081/github.com/gorilla/mux/@v/list
```

Cached content is served (for module lists and `@latest` even past their cache expiry, since there is nothing to refresh from); anything not in the cache is answered with `410 Gone` and a `Disable-Module-Fetch: true` response header. The flag `-disableModuleFetch` enables the mode for all requests, and the header is echoed on served responses.

### Cache garbage collection

The download cache grows unbounded by default: cached module files and sumdb entries are immutable and never expire on disk. To enable periodic cleanup, set `-gcInterval` (it is `0`, disabled, by default). Every interval, files in the cache whose **last access time** is older than `-gcKeep` (default 14 days) are deleted, and directories that became empty are pruned:

```shell
./bin/goproxy -listen=0.0.0.0:80 -cacheDir=/var/lib/goproxy -gcInterval=1h -gcKeep=720h
```

The sweep is safe against a running proxy: freshly fetched files have a recent access time, and non-empty directories are never removed.

### Private module authentication

Some private modules are gated behind `git` authentication. To resolve this, you can force git to rewrite the URL with a personal access token present for auth

```shell
git config --global url."https://${GITHUB_PERSONAL_ACCESS_TOKEN}@github.com/".insteadOf https://github.com/
```

This can be done for other git providers as well, following the same pattern

## Build docker image

If you want to build the docker image locally (you don't have to):

```shell
docker build -t goproxy/goproxy:latest .
```

The image architecture is detected automatically. To force one, pass `--build-arg ARCH=amd64` (or `arm64`).

## Use docker image

```shell
docker run -d -p80:8081 goproxy/goproxy
```

Published multi-arch (`linux/amd64`, `linux/arm64`) images for this fork are on GHCR:

```shell
docker run -d -p80:8081 ghcr.io/wovan/goproxy:latest
```

Use the -v flag to persisting the proxy module data (change ___cacheDir___ to your own dir):

```
docker run -d -p80:8081 -v cacheDir:/go goproxy/goproxy
```

## Docker Compose

```shell
docker-compose up
```

## Kubernetes

Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: goproxy
  name: goproxy
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: goproxy
    spec:
      containers:
      - args:
        - -proxy
        - https://goproxy.io
        - -listen
        - 0.0.0.0:8081
        - -cacheDir
        - /tmp/test
        - -exclude
        - github.com/my-org/*
        image: goproxy/goproxy
        name: goproxy
        ports:
        - containerPort: 8081
        volumeMounts:
        - mountPath: /tmp/test
          name: goproxy
      volumes:
      - emptyDir:
          medium: Memory
          sizeLimit: 500Mi
        name: goproxy
```

Deployment (with gitconfig secret):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: goproxy
  name: goproxy
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: goproxy
    spec:
      containers:
      - args:
        - -proxy
        - https://goproxy.io
        - -listen
        - 0.0.0.0:8081
        - -cacheDir
        - /tmp/test
        - -exclude
        - github.com/my-org/*
        image: goproxy/goproxy
        name: goproxy
        ports:
        - containerPort: 8081
        volumeMounts:
        - mountPath: /tmp/test
          name: goproxy
        - mountPath: /root
          name: gitconfig
          readOnly: true
      volumes:
      - emptyDir:
          medium: Memory
          sizeLimit: 500Mi
        name: goproxy
      - name: gitconfig
        secret:
          secretName: gitconfig
---
apiVersion: v1
data:
  # NOTE: Encoded version of the following, replacing ${GITHUB_PERSONAL_ACCESS_TOKEN}
  # [url "https://${GITHUB_PERSONAL_ACCESS_TOKEN}@github.com/"]
  # insteadOf = https://github.com/
  .gitconfig: *****************************
kind: Secret
metadata:
  name: test
```

## Appendix

- If running locally, set `export GOPROXY=http://localhost[:PORT]` to use your goproxy.
- Set `export GOPROXY=direct` to directly access modules without your goproxy.
