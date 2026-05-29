GO ?= go
BIN ?= bin/claude-msg
PREFIX ?= /usr/local

.PHONY: build test vet install clean

build: $(BIN)

$(BIN): $(shell find . -name '*.go' -not -name '*_test.go' -not -path './bin/*') go.mod go.sum
	@mkdir -p $(dir $(BIN))
	$(GO) build -o $(BIN) ./cmd/claude-msg

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
