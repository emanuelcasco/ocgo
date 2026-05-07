.PHONY: build run test clean install release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOBIN := $(shell go env GOBIN)
GOPATH := $(shell go env GOPATH)
INSTALL_DIR := $(if $(GOBIN),$(GOBIN),$(GOPATH)/bin)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ocgo ./cmd/ocgo

run:
	go run ./cmd/ocgo

test:
	go test ./...

clean:
	rm -rf bin

install: build
<<<<<<< Updated upstream
	install -m 0755 bin/ocgo $(HOME)/go/bin/ocgo
||||||| Stash base
	install -m 0755 bin/ocgo $$(go env GOBIN)/ocgo
=======
	mkdir -p "$(INSTALL_DIR)"
	install -m 0755 bin/ocgo "$(INSTALL_DIR)/ocgo"
>>>>>>> Stashed changes

release:
	@[ -n "$(TAG)" ] || (echo "Usage: make release TAG=v0.1.0" && exit 1)
	./scripts/release.sh "$(TAG)"
