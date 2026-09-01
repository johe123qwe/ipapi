GO ?= go
ADDR ?= :8080
ARCH ?= amd64

.PHONY: all data fetch build run clean dist release

all: build

## fetch: download the source databases into data/src
fetch:
	./scripts/fetch.sh

## data: (re)build data/mmdb/geo.mmdb and data/mmdb/asn.mmdb
data: fetch
	$(GO) run ./cmd/build -src data/src -out data/mmdb

## build: compile the server binary
build:
	$(GO) build -o bin/ipapi-server ./cmd/server

## run: start the API on $(ADDR)
run: build
	./bin/ipapi-server -addr $(ADDR)

## dist: cross-compile static Linux binaries into dist/
dist:
	@mkdir -p dist
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 $(GO) build -ldflags="-s -w" -o dist/ipapi-server ./cmd/server
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 $(GO) build -ldflags="-s -w" -o dist/ipapi-build  ./cmd/build

## release: build dist/ipapi-linux-$(ARCH).tar.gz for deployment
release: dist
	@rm -rf dist/pkg && mkdir -p dist/pkg/bin dist/pkg/scripts dist/pkg/deploy
	cp dist/ipapi-server dist/ipapi-build dist/pkg/bin/
	cp scripts/fetch.sh dist/pkg/scripts/
	cp deploy/*.service deploy/*.timer deploy/install.sh deploy/update.sh deploy/nginx.conf dist/pkg/deploy/
	cp README.md dist/pkg/
	tar -C dist/pkg -czf dist/ipapi-linux-$(ARCH).tar.gz .
	@echo "==> dist/ipapi-linux-$(ARCH).tar.gz"

clean:
	rm -rf bin dist data/mmdb/*.mmdb
