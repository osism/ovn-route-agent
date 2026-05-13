BINARY    := ovn-network-agent
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOFLAGS   := -trimpath

.PHONY: all build build-static clean fmt vet test test-integration install docs-gen docs-gen-check e2e-images e2e-up e2e-down e2e-install-tools

# Containerlab E2E harness. See test/e2e/README.md for the topology and
# acceptance criteria (issue #44).
E2E_TOPOLOGY    := test/e2e/topology.clab.yml
E2E_BOOTSTRAP   := test/e2e/bootstrap.sh
E2E_GWNODE_TAG  := ovn-network-agent/gwnode:e2e
E2E_CENTRAL_TAG := ovn-network-agent/central:e2e

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

# Integration tests exercise the agent against a real OVN/OVS/FRR/nftables
# stack. They require Linux + root (CAP_NET_ADMIN). See
# docs/contributing/integration-tests.md (published at
# https://osism.github.io/ovn-network-agent/contributing/integration-tests)
# for local-run prerequisites.
test-integration: build
	OVN_AGENT_BINARY=$(CURDIR)/$(BINARY) go test -tags=integration -v -count=1 ./test/integration/...

clean:
	rm -f $(BINARY)

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)

# Regenerate docs/reference/{configuration,cli,metrics}.md from the
# canonical Go declarations in config.go and metrics.go. See
# tools/docgen for the implementation. Run after touching either
# file or the agent's flag/env/YAML surface.
docs-gen:
	go run ./tools/docgen

# Fail if the generated reference pages are out of date. Used in CI
# so PRs that touch config.go or metrics.go without regenerating the
# reference docs are caught before merge.
docs-gen-check: docs-gen
	@git diff --exit-code -- docs/reference/ || ( \
		echo ""; \
		echo "docs/reference/ is out of date — run 'make docs-gen' and commit the result."; \
		exit 1; \
	)

# Build the containerlab E2E images for the host platform. The gwnode
# Dockerfile builds the agent from source via a Go build stage, so no
# pre-build of the binary is required. Plain `docker build` is used so
# the target works on hosts without the docker-buildx-plugin (which is
# only required for multi-arch publication, documented in
# docs/contributing/e2e-tests.md).
e2e-images:
	docker build -f test/e2e/Dockerfile.central -t $(E2E_CENTRAL_TAG) .
	docker build -f test/e2e/Dockerfile.gwnode  -t $(E2E_GWNODE_TAG)  .

# Install the containerlab CLI when it is missing. Linux only: the
# upstream project publishes Linux binaries and a one-line installer
# (https://get.containerlab.dev) for Debian/RHEL hosts and ships no
# darwin binary, so on macOS the recommended path is to run
# containerlab inside a Linux VM — see
# https://containerlab.dev/macos/. This target reports that and
# exits with a non-zero status on macOS instead of pretending to
# install something.
e2e-install-tools:
	@if command -v containerlab >/dev/null 2>&1; then \
		echo "containerlab already installed: $$(command -v containerlab)"; \
	elif [ "$$(uname -s)" = "Linux" ]; then \
		echo "installing containerlab via the upstream installer (needs sudo)"; \
		bash -c "$$(curl -sL https://get.containerlab.dev)"; \
	elif [ "$$(uname -s)" = "Darwin" ]; then \
		echo ""; \
		echo "containerlab does not ship a native macOS binary."; \
		echo "Run the E2E lab from a Linux host or a Linux VM"; \
		echo "(OrbStack, Docker Desktop's Linux VM, Colima, ...)"; \
		echo "See https://containerlab.dev/macos/ for the recommended setup."; \
		exit 1; \
	else \
		echo "unsupported platform $$(uname -s); install containerlab manually from https://containerlab.dev/install/"; \
		exit 1; \
	fi

# Bring the containerlab E2E lab up: build the images, deploy the
# topology, and seed the OVN NB DB with the canned state. Errors out
# with a pointer to `make e2e-install-tools` when containerlab is
# missing.
e2e-up: e2e-images
	@command -v containerlab >/dev/null 2>&1 || ( \
		echo ""; \
		echo "containerlab is not on PATH."; \
		echo "Run 'make e2e-install-tools' once to install it, then retry."; \
		exit 1; \
	)
	containerlab deploy -t $(E2E_TOPOLOGY)
	$(E2E_BOOTSTRAP)

# Tear the containerlab E2E lab down.
e2e-down:
	containerlab destroy -t $(E2E_TOPOLOGY) --cleanup
