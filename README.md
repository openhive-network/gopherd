# WORK IN PROGRESS SOFTWARE, DO NOT USE YET 

# ghive


A Go rewrite of [hived](https://gitlab.syncad.com/hive/hive) — the Hive blockchain daemon.
Goal: faster replay/sync and lower memory than the C++ implementation while remaining
config.ini-compatible. Pure Go, no cgo.

**Status:** Pre-P0 scaffold. Not usable yet. See `~/.claude/plans/look-at-hive-i-encapsulated-starlight.md`
for the full roadmap.

## Requirements

- Go 1.26.2 or newer.

## Layout

```
cmd/ghive             — daemon entrypoint
cmd/ghive-tools       — operator/dev CLI: blockid, inspect, convert
internal/protocol     — pure consensus types (no DB)
internal/fcraw        — FC binary codec (Hive's wire format)
internal/crypto       — sha256, ripemd160, secp256k1, base58 STM keys
internal/blocklog     — hived-compatible block_log reader
internal/state        — Pebble-backed state with chainbase undo semantics (P1+)
internal/chain        — apply_block / evaluators (P1+)
internal/p2p          — graphene-compatible P2P (P2+)
internal/api          — JSON-RPC server + plugins (P3+)
internal/plugins      — witness production etc (P4+)
```

## Build

```
make           # builds bin/ghive and bin/ghive-tools
make test      # runs all tests
make vet       # static analysis
```

## Roadmap

- **P0** Foundations: types, fcraw codec, crypto, blocklog reader, `blockid` tool.
- **P1** Replay-only mode (headline benchmark vs `hived --replay-blockchain`).
- **P2** Passive P2P sync.
- **P3** Read-side JSON-RPC parity.
- **P4** Witness production.
- **P5** Hardening + `ghive convert` tool.

## License

TBD — to match hived's upstream license once the project is published.
