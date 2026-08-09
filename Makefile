.PHONY: all build test vet fmt clean tidy

GO ?= go
BIN_DIR := $(CURDIR)/bin

all: build

# The CLI was consolidated into a single `gopherd <subcommand>` binary; the old
# ghive / ghive-tools / drift-hunter targets pointed at directories that no
# longer exist, so `make build` failed outright.
build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/gopherd ./cmd/gopherd

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
