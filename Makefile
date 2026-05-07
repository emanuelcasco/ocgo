.PHONY: build run test clean install release

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/ocgo ./cmd/ocgo

run:
	go run ./cmd/ocgo

test:
	go test ./...

clean:
	rm -rf bin

install: build
	install -m 0755 bin/ocgo $$(go env GOBIN)/ocgo

release:
	@[ -n "$(TAG)" ] || (echo "Usage: make release TAG=v0.1.0" && exit 1)
	./scripts/release.sh "$(TAG)"
