.PHONY: build run test clean install release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
EXE := $(shell go env GOEXE)
OCGO_BIN := bin/ocgo$(EXE)
GOBIN := $(shell go env GOBIN)
GOPATH := $(shell go env GOPATH)
INSTALL_DIR := $(if $(GOBIN),$(GOBIN),$(GOPATH)/bin)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(OCGO_BIN) ./cmd/ocgo

run:
	go run ./cmd/ocgo

test:
	go test ./...

clean:
	rm -rf bin

install: build
	mkdir -p "$(INSTALL_DIR)"
	install -m 0755 "$(OCGO_BIN)" "$(INSTALL_DIR)/ocgo$(EXE)"

release:
	@[ -n "$(TAG)" ] || (echo "Usage: make release TAG=v0.1.0" && exit 1)
	./scripts/release.sh "$(TAG)"
