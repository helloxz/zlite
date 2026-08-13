VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/helloxz/zlite/internal/version.Version=$(VERSION) \
	-X github.com/helloxz/zlite/internal/version.Commit=$(COMMIT) \
	-X github.com/helloxz/zlite/internal/version.BuildTime=$(BUILD_TIME)

BIN := bin/zlite

.PHONY: build test vet run clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/zlite

test:
	go test ./...

vet:
	go vet ./...

run: build
	./$(BIN)

clean:
	rm -rf bin
