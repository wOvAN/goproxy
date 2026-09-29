.PHONY: build image clean test lint

export GO111MODULE=on

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

all: build

build: tidy
	@go build -o bin/goproxy -ldflags "-s -w -X main.version=$(VERSION)" .

tidy:
	@go mod tidy

image:
	@docker build -t goproxy/goproxy .

test: tidy
	@go test -v ./...

lint:
	@golangci-lint run ./...

fmt:
	@go fmt ./...

fix:
	@go fix -v ./...

clean:
	@git clean -f -d -X
