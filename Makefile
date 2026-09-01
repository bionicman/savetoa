GO ?= go
BINARY := build/savetoa

.PHONY: all build test fmt vet check clean deb

all: check build

build:
	mkdir -p build
	$(GO) build -trimpath -o $(BINARY) ./cmd/savetoa

test:
	$(GO) test ./...

fmt:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -type f))" || \
		(echo "Go files are not formatted; run gofmt -w on the listed files"; \
		gofmt -l $$(find . -name '*.go' -type f); exit 1)

vet:
	$(GO) vet ./...

check: fmt vet test

deb:
	dpkg-buildpackage -us -uc -b

clean:
	rm -rf build
