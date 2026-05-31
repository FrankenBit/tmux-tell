GO ?= go
BIN ?= bin/claude-msg
PREFIX ?= /usr/local

# Version stamping. VERSION defaults to `git describe` so an untagged
# build between releases shows e.g. v0.1.0-3-g4f5e6d7 (dirty if there
# are uncommitted changes). Override at build time:
#   make build VERSION=v0.2.0-rc1
# Plain `go build` (no Makefile) picks up the source default ("dev").
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -X git.frankenbit.de/frankenbit/cli-semaphore/internal/version.Version=$(VERSION)

.PHONY: build test vet install clean version check-pin-slugs

version:
	@echo $(VERSION)

# check-pin-slugs enforces ADR-0001's discipline-pin slug register
# against the slugs actually in use across the codebase (#51). Runs
# as part of CI; surface here so the operator can run it locally
# before pushing.
check-pin-slugs:
	$(GO) run ./tools/check-pin-slugs/

build: $(BIN)

$(BIN): $(shell find . -name '*.go' -not -name '*_test.go' -not -path './bin/*') go.mod go.sum
	@mkdir -p $(dir $(BIN))
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/claude-msg

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
