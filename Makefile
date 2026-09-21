GO ?= go
GOFMT ?= gofmt
VERSION ?= dev
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || printf unknown)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || printf 0)
DATE := $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build check fmt fmt-check test vet dist clean

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/garm-provider-timeweb ./cmd/garm-provider-timeweb

fmt:
	$(GOFMT) -w .

fmt-check:
	@files=$$($(GOFMT) -l .); if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

test:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

check: fmt-check test vet build

dist:
	GO='$(GO)' VERSION='$(VERSION)' COMMIT='$(COMMIT)' SOURCE_DATE_EPOCH='$(SOURCE_DATE_EPOCH)' bash scripts/build-release.sh

clean:
	rm -rf bin dist
