BINARY    := ovn-network-agent
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOFLAGS   := -trimpath

.PHONY: all build build-static clean fmt vet test install

all: build

build:
	GOOS=linux go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

# Static binary for deployment on minimal systems
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test -v ./...

clean:
	rm -f $(BINARY)

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)
