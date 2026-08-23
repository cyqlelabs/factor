BINARY    := factor
MODULE    := github.com/cyqlelabs/factor
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDTIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILDTIME)

# svu derives the next semantic version from the Conventional Commits since the
# last tag; the bump rules live in .svu.yml. Pinned so a new svu release can't
# silently change what CI tags.
SVU ?= go run github.com/caarlos0/svu/v3@v3.4.1

export CGO_ENABLED=0

COVER_MIN := 90

.PHONY: build build-all build-tiny install test test-race cover vet fmt lint check clean version version-next win-setup test-windows test-windows-race

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

# The race detector needs cgo; release builds stay CGO-free.
test-race:
	CGO_ENABLED=1 go test -race ./...

# Statement coverage across every package that has tests, gated at COVER_MIN.
# Test-less packages are listed out explicitly: they contribute nothing to the
# profile, and some Go installs ship without the covdata tool they'd need.
cover:
	go test -coverprofile=coverage.out $$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)
	@go tool cover -func=coverage.out | tail -1
	@total=$$(go tool cover -func=coverage.out | tail -1 | awk '{print $$3}' | tr -d '%'); \
	awk -v t="$$total" -v min="$(COVER_MIN)" 'BEGIN { if (t+0 < min+0) { printf "FAIL: coverage %.1f%% is below the %s%% minimum\n", t, min; exit 1 } printf "coverage %.1f%% meets the %s%% minimum\n", t, min }'

# The Windows gate. CI only ever cross-compiles the Windows target, so until
# these run, ~1000 lines across 12 _windows.go files have never executed. The
# suite runs inside a VirtualBox guest, in its logged-on session — a desktop
# screenshot and a tray icon have nowhere to land in the session ssh gets.
# `make win-setup` provisions the guest and captures the snapshot once;
# `make test-windows` restores that snapshot and runs the suite on it.
win-setup:
	./test/windows/run.sh setup

test-windows:
	./test/windows/run.sh ci

test-windows-race:
	./test/windows/run.sh race

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	golangci-lint run

check: vet test-race cover
	@gofmt -l . | tee /dev/stderr | wc -l | grep -q '^0$$'

# The version this tree builds as (the last tag), and the version CI will tag
# next. Equal output means nothing since the last tag warrants a release.
version:
	@$(SVU) current

version-next:
	@$(SVU) next

clean:
	rm -rf dist $(BINARY) $(BINARY)-tiny
