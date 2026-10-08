# scripts/

Helpers for running the C++ `hived` reference node alongside `ghive` for
parity work — querying authoritative state at any past block without
recompiling hived (project rule).

## hived-at — one-shot wrapper around the checkpoint framework

`hived-at <BLOCK> <subcommand>` collapses the previous five-step manual
flow (prepare → serve → wait → curl → kill) into a single command. It
prepares the checkpoint dir if missing, starts the API server if not
already running for that block, waits until the server responds, and
issues your query.

```
hived-at <BLOCK> dgpo                     # database_api.get_dynamic_global_properties
hived-at <BLOCK> witness <NAME>           # database_api.find_witnesses
hived-at <BLOCK> account <NAME>           # database_api.find_accounts
hived-at <BLOCK> schedule                 # database_api.get_witness_schedule
hived-at <BLOCK> raw <method> [<params>]  # arbitrary JSON-RPC call (params is JSON)
hived-at <BLOCK> shell                    # ensure server up; print URL+PID; exit
hived-at <BLOCK> stop                     # cleanly kill server, remove pid file
hived-at list                             # show all checkpoint dirs + status
```

### Reuse + concurrency

* The first call for a given BLOCK will run `hived-prepare-checkpoint.sh`
  (slow) and start hived. Subsequent calls reuse the prepared dir and the
  running server — typically <100 ms round-trip.
* Each block binds to its own port, computed as `BASE_PORT + (BLOCK % 1000)`
  (default base: `18090`). Two `hived-at` calls for *different* blocks can
  coexist. Override with `HIVED_AT_PORT=<n>` if a port is taken.
* Per-checkpoint state lives in `/tmp/hived-cp-<BLOCK>/`:
  * `server.pid`  — hived's PID
  * `server.port` — bound port
  * `server.log`  — hived stdout/stderr
  * `api-config.ini` — generated config (port + p2p hardened)

### Env overrides

| Var | Default | Purpose |
| --- | --- | --- |
| `HIVED_BIN` | `/home/martin/projects/hive/build/programs/hived/hived` | path to hived |
| `HIVED_AT_BASE_PORT` | `18090` | base TCP port |
| `HIVED_AT_PORT` | (unset) | override port entirely |
| `HIVED_AT_WAIT` | `60` | seconds to wait for API up |
| `HIVED_AT_PREPARE` | `scripts/hived-prepare-checkpoint.sh` | path to prepare script |

### Example: confirm a witness's signing_key

```
$ hived-at 5148656 witness gxt-1080-sc-0028 | jq '.witnesses[0].signing_key'
"STM1111111111111111111111111111111114T1Anm"
```

(That's the disabled-witness sentinel — relevant to the iter14 schedule
investigation.)

## Other scripts

| Script | Purpose |
| --- | --- |
| `hived-prepare-checkpoint.sh` | Build a `/tmp/hived-cp-<N>/` dir whose block_log is truncated to N and whose state is replayed to N. (Called by `hived-at`.) |
| `hived-serve-checkpoint.sh`   | Start hived in API mode against a prepared checkpoint, foreground. (Superseded by `hived-at`.) |
| `hived-replay-to.sh`          | Replay hived to N + dump a state_snapshot. Older snapshot-based path. |
| `hived-api-serve.sh`          | Start hived API by reloading a snapshot dumped by `hived-replay-to.sh`. |
| `hived-api.conf`              | Plugin/port/p2p config used by `hived-serve-checkpoint.sh`. |
| `parity-replay-compare.sh`    | Replay one block_log with gopherd AND a mainnet hived, then diff the two states field by field (dgpo, feed, every witness and account, orders, conversions, savings withdrawals, delegation returns). Memory-capped via `systemd-run`. |
| `parity-compare-state.py`     | The diff itself: gopherd `state-jsonl` dump vs a hived `database_api` endpoint. Exit 1 on any mismatch. Used by `parity-replay-compare.sh`, usable alone against any running hived. |
