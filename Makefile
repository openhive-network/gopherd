.PHONY: all build test test-race check fmt-check vet tidy-check fuzz cover test-replay fmt clean tidy

GO ?= go
BIN_DIR := $(CURDIR)/bin
# Per-target fuzzing time for `make fuzz` (go test -fuzztime syntax).
FUZZTIME ?= 30s
# Parallelism for the race run. Race-built tests take 5-10x their normal
# memory: the chain fixture package peaks at 6.5 GB even one test at a time,
# so the defaults run one package and one test at a time (about 7 GB).
RACE_P ?= 1
RACE_PARALLEL ?= 1

all: build

# The CLI was consolidated into a single `gopherd <subcommand>` binary; the old
# ghive / ghive-tools / drift-hunter targets pointed at directories that no
# longer exist, so `make build` failed outright.
build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/gopherd ./cmd/gopherd

# Unit tests: no network, no block_log, a couple of minutes. Includes the
# checked-in fuzz corpora under testdata/fuzz as regression inputs.
test:
	$(GO) test ./...

test-race:
	$(GO) test -race -p $(RACE_P) -parallel $(RACE_PARALLEL) ./...

# What every change must pass before it is pushed; CI runs the same.
check: fmt-check vet tidy-check test

fmt-check:
	@out=$$(gofmt -l ./cmd ./internal ./test 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

tidy-check:
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak; \
	$(GO) mod tidy; rc=0; \
	cmp -s go.mod go.mod.bak && cmp -s go.sum go.sum.bak || { echo "go.mod/go.sum not tidy: run make tidy"; rc=1; }; \
	mv go.mod.bak go.mod; mv go.sum.bak go.sum; exit $$rc

# Runs every Fuzz* target for FUZZTIME each (make fuzz FUZZTIME=10m).
fuzz:
	FUZZTIME=$(FUZZTIME) GO=$(GO) ./scripts/fuzz.sh

cover:
	$(GO) test -coverprofile=coverage.txt ./...
	$(GO) tool cover -func=coverage.txt | tail -1

# Replays the start of a mainnet block_log and checks chain totals against
# recorded checkpoints. Needs GOPHERD_BLOCK_LOG=/path/to/block_log.
test-replay:
	$(GO) test -tags replay -run TestReplayCheckpoints -timeout 6h -v ./test/replay

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
