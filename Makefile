# usage -- multi-provider coding-plan usage monitor
# ==================================================
# Build, install, test, run.

BINARY  := usage
PKG     := ./cmd/usage
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"
GO      := go

.PHONY: all build install test fmt vet run clean tidy release

all: build

build:
	$(GO) build $(LDFLAGS) -o $(BINARY) $(PKG)

install: build
	install -Dm755 $(BINARY) $(HOME)/.local/bin/$(BINARY)
	@echo "Installed to ~/.local/bin/$(BINARY)"

tidy:
	$(GO) mod tidy

release:
	$(GO) build $(LDFLAGS) -trimpath -o $(BINARY) $(PKG)
	@echo "Release build: $(BINARY)"

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

run:
	$(GO) run $(PKG) show

clean:
	rm -f $(BINARY)
