GO ?= go
BINDIR ?= bin
PREFIX ?= /usr/local

# Adapter binaries live in cmd/tmux-msg-<adapter>/ (#177): the binary name
# encodes the substrate (tmux-msg) + the CLI-tool adapter (claude today; codex /
# copilot if those ever materialize). `make build` builds every adapter under
# cmd/tmux-msg-*/; a future cmd/tmux-msg-codex/ is picked up with no Makefile
# change. `make build-claude` builds just the Claude adapter.
ADAPTERS := $(notdir $(wildcard cmd/tmux-msg-*))

# Version stamping. VERSION defaults to `git describe` so an untagged
# build between releases shows e.g. v0.1.0-3-g4f5e6d7 (dirty if there
# are uncommitted changes). Override at build time:
#   make build VERSION=v0.2.0-rc1
# Plain `go build` (no Makefile) picks up the source default ("dev").
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -X git.frankenbit.de/frankenbit/tmux-msg/internal/version.Version=$(VERSION)

.PHONY: build build-claude test vet install clean version check-pin-slugs

version:
	@echo $(VERSION)

# check-pin-slugs enforces ADR-0001's discipline-pin slug register
# against the slugs actually in use across the codebase (#51). Runs
# as part of CI; surface here so the operator can run it locally
# before pushing.
check-pin-slugs:
	$(GO) run ./tools/check-pin-slugs/

# build builds every adapter binary. Each lands at bin/<adapter> via the
# pattern rule below. CI's `go build ./...` independently compiles every
# cmd/* main, so the matrix is covered there too.
build: $(ADAPTERS:%=$(BINDIR)/%)

# build-claude builds just the Claude Code adapter — the common case.
build-claude: $(BINDIR)/tmux-msg-claude

# Pattern rule: bin/tmux-msg-<x> is built from cmd/tmux-msg-<x>/. $(@F) is the
# target's basename (e.g. tmux-msg-claude), which is also the cmd/ subdir name.
GO_SOURCES := $(shell find . -name '*.go' -not -name '*_test.go' -not -path './bin/*')
$(BINDIR)/tmux-msg-%: $(GO_SOURCES) go.mod go.sum
	@mkdir -p $(BINDIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $@ ./cmd/$(@F)

vet:
	$(GO) vet ./...

test:
	$(GO) test -race -count=1 ./...

# install runs install.sh — needs root, so the user invokes:
#   make build && sudo PREFIX=$(PREFIX) ./install.sh
# This target is a convenience wrapper that uses sudo -A so the alcatraz
# tmux-popup askpass surfaces the prompt cleanly.
install: build
	sudo -A PREFIX=$(PREFIX) ./install.sh

clean:
	rm -rf bin/
