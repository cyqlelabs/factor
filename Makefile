BINARY    := factor
MODULE    := github.com/cyqlelabs/factor
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDTIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILDTIME)

export CGO_ENABLED=0

.PHONY: build build-all build-tiny install test test-race vet fmt lint check clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/factor

# Cross-compile the release set. GOAMD64=v1 keeps the amd64 binary running
# on old CPUs without SSE4.2 (e.g. low-resource Puppy Linux boxes).
build-all: clean
	mkdir -p dist
	GOOS=linux  GOARCH=amd64 GOAMD64=v1 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 ./cmd/factor
	GOOS=linux  GOARCH=386                go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-386 ./cmd/factor
	GOOS=linux  GOARCH=arm64              go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 ./cmd/factor
	GOOS=linux  GOARCH=arm GOARM=7        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-armv7 ./cmd/factor
	GOOS=darwin GOARCH=arm64              go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 ./cmd/factor
	GOOS=windows GOARCH=amd64             go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe ./cmd/factor

# Smallest binary: browser suite stripped.
build-tiny:
	GOAMD64=v1 go build -trimpath -tags nobrowser -ldflags "$(LDFLAGS)" -o $(BINARY)-tiny ./cmd/factor

install: build
	install -m 0755 $(BINARY) $(HOME)/.local/bin/$(BINARY)

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	golangci-lint run

check: vet test-race
	@gofmt -l . | tee /dev/stderr | wc -l | grep -q '^0$$'

clean:
	rm -rf dist $(BINARY) $(BINARY)-tiny
