.PHONY: all build test vet fmt clean tidy

GO ?= go
BIN_DIR := $(CURDIR)/bin

all: build

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/ghive ./cmd/ghive
	$(GO) build -o $(BIN_DIR)/ghive-tools ./cmd/ghive-tools

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
