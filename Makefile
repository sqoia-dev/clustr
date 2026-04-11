VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags="-X main.version=$(VERSION) -s -w"

.PHONY: all client server static clean test

all: client server

client:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/clonr ./cmd/clonr

server:
	go build $(LDFLAGS) -o bin/clonr-serverd ./cmd/clonr-serverd

# static builds a fully static binary suitable for embedding in PXE initramfs.
# Uses -a to force rebuild of all packages with CGO disabled.
static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -a -o bin/clonr-static ./cmd/clonr

test:
	go test ./... -v

clean:
	rm -rf bin/
