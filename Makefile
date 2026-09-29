.PHONY: build image clean test lint

export GO111MODULE=on

all: build

build: tidy
	@go build -o bin/goproxy -ldflags "-s -w" .

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
