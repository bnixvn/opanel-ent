# OPanel Enterprise build
#
# CGO_ENABLED=0 is not optional: it is what produces a single static binary
# with no libc or Python dependency, which is the whole reason the panel can
# sit beside CloudLinux's own Python tooling without conflicting with it.

BINARIES  := opanel-api opanel-agent opanelctl
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG       := github.com/bnixvn/opanel-ent/internal/version
LDFLAGS   := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

GO        ?= go
DIST      ?= dist

export CGO_ENABLED = 0

.PHONY: all build web linux test lint vet fmt tidy clean check FORCE

all: check build

build: $(BINARIES:%=$(DIST)/%)

# FORCE rather than a list of Go sources, and never .PHONY: make does not
# apply pattern rules to phony targets, so marking these phony removes the
# only recipe that builds them and `make build` does nothing at all.
#
# Without either, a pattern rule with no prerequisites is satisfied the
# moment the file exists: `make build` stopped rebuilding after the first
# run and shipped the previous binary, which is the worst way for a build to
# fail because it looks like it worked.
#
# go build is the right thing to decide what needs doing -- it already knows
# what changed, and its cache makes a no-op rebuild cost about a second.
$(DIST)/%: FORCE
	@mkdir -p $(DIST)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/$*

# An empty rule with no prerequisites of its own: anything depending on it is
# always out of date.
FORCE:

# The interface. Its build output lands in internal/httpapi/web, which is
# committed, so `go build` works on a clone that has never seen npm -- the
# binary embeds that directory and there is no second artifact to ship.
web:
	cd web && npm ci && npm run build

# Cross-compile for the target host from any workstation.
linux:
	@mkdir -p $(DIST)/linux-amd64
	@for b in $(BINARIES); do \
		GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o $(DIST)/linux-amd64/$$b ./cmd/$$b || exit 1; \
	done
	@ls -lh $(DIST)/linux-amd64

test:
	$(GO) test ./... -count=1

race:
	CGO_ENABLED=1 $(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

lint:
	golangci-lint run

check: vet test

clean:
	rm -rf $(DIST)
