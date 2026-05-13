BINARY    := ovn-network-agent
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOFLAGS   := -trimpath

.PHONY: all build build-static clean fmt vet test test-integration install docs-gen docs-gen-check e2e-images e2e-up e2e-down

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
# pre-build of the binary is required.
e2e-images:
	docker buildx build --load \
		-f test/e2e/Dockerfile.central -t $(E2E_CENTRAL_TAG) .
	docker buildx build --load \
		-f test/e2e/Dockerfile.gwnode  -t $(E2E_GWNODE_TAG)  .

# Bring the containerlab E2E lab up: build the images, deploy the
# topology, and seed the OVN NB DB with the canned state.
e2e-up: e2e-images
	containerlab deploy -t $(E2E_TOPOLOGY)
	$(E2E_BOOTSTRAP)

# Tear the containerlab E2E lab down.
e2e-down:
	containerlab destroy -t $(E2E_TOPOLOGY) --cleanup
