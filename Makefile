# em-xray — build / dev tasks
#
# After a fresh clone, run `make fetch-xray` once to download the embedded xray
# binary (gitignored, ~40MB) before building. Cross-compiling for another OS?
# pass TARGET to match, e.g. `make fetch-xray TARGET=macos-arm64`.

BIN        := emx
PKG        := ./cmd/emx
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
XRAY_VERSION ?= v26.3.27
TARGET     ?= linux-64

GOBIN := $(shell go env GOPATH)/bin

.PHONY: all fetch-xray proto build run test vet fmt clean tools release

all: build

## fetch-xray: download + embed the xray binary and geo data for TARGET
fetch-xray:
	XRAY_VERSION=$(XRAY_VERSION) TARGET=$(TARGET) ./scripts/fetch-xray.sh

## proto: regenerate gRPC stubs from api/emx.proto
proto:
	PATH="$(GOBIN):$$PATH" protoc --proto_path=api \
		--go_out=api/emxv1 --go_opt=paths=source_relative \
		--go-grpc_out=api/emxv1 --go-grpc_opt=paths=source_relative \
		emx.proto

## build: build the emx binary for the host
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

## run: build + start the daemon in the foreground
run: build
	./$(BIN) start --foreground

## test: run unit tests (core/ is OS-agnostic and runs anywhere)
test:
	go test ./...

## vet: go vet the module
vet:
	go vet ./...

## fmt: gofmt the tree
fmt:
	gofmt -w .

## release: bump + push a release tag (triggers the release workflow)
##   make release BUMP=patch   (or minor|major|vX.Y.Z)
release:
	./scripts/release.sh $(BUMP)

## tools: install protoc plugins
tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

clean:
	rm -f $(BIN)
	rm -rf dist
