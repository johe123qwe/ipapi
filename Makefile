GO ?= go
ADDR ?= :8080

.PHONY: all data fetch build run clean

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

clean:
	rm -rf bin data/mmdb/*.mmdb
