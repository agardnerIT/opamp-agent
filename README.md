# opamp-agent — the generic OpAMP bridge

![Warning: Entirely vibecoded](https://img.shields.io/badge/Warning-Entirely%20vibecoded-orange?style=for-the-badge)

> **Status: working, proven end-to-end against a live OpAMP server.** One
> binary that speaks standard OpAMP on behalf of a product that doesn't.

## Are you a vendor trying to add OpAMP support for your product?

**Then this is not what you're looking for.** The right way to support OpAMP
is to implement it *natively* — embed an OpAMP client library in your agent
([opamp-go](https://github.com/open-telemetry/opamp-go),
opamp-java, opamp-python, opamp-rust, …) and speak the
[OpAMP spec](https://opentelemetry.io/docs/opamp/) directly over its standard
transport. That is a first-class integration: no sidecar, full control of your
identity, capabilities, and lifecycle, and it's what the spec was designed
for. Start small — capabilities negotiation lets you ship identity + health
only, and add remote config later.

This bridge exists for everything that *can't* or *won't* embed a client yet:
products whose management surface is external (config files, signals, control
sockets, HTTP admin endpoints) and whose maintainers want standard OpAMP
management without writing a protocol implementation. If that's you, you
write one small driver (or, for file-and-restart products, just a YAML
manifest) — see below.

## What this is

One binary that speaks OpAMP on behalf of a product. All product-agnostic
work — OpAMP transport, enrollment, identity, heartbeats, health reporting,
config persistence, rollback — lives here, written once, audited once:

```
driver/driver.go     Interface 2 — the six-verb contract (the vendor-facing surface)
bridge/bridge.go     The commodity layer: opamp-go transport + the LKG
                     render→apply→verify pipeline
drivers/             Compiled-in drivers
  proc/              Manifest-driven driver for config-file + reload products
                     (no product-specific code — a manifest is the whole
                     vendor artifact; see examples/prometheus.yaml)
cmd/opamp-agent/     Binary; drivers are compiled in and selected by flag
captures-prometheus.md  Evidence trail for the proc driver's end-to-end proof
```

## The six questions a driver answers

| Verb | Question |
|---|---|
| `Identity` | who are you, which files do you own? |
| `Render` | what goes on disk for this config? |
| `Validate` | will the product accept it pre-apply? (`ErrUnsupported` is an honest answer) |
| `Apply` | how does it take effect? (signal / reload API / control socket) |
| `Verify` | did it ACTUALLY take effect? (apply responses can lie) |
| `Health` / `EffectiveConfig` | alive? what's running? |

## What the bridge guarantees (so drivers never re-invent it)

- OpAMP transport, enrollment, identity, capabilities, health reporting
- **LKG recovery**: before every render it snapshots the driver's owned
  files; if verification fails it restores them and re-applies before
  reporting FAILED — a rejected push never leaves the product disarmed
- Apply responses are never trusted as verification — `Verify` is the only
  source of truth
- The bridge owns artifact file writes; drivers only activate
- Pipeline ordering: artifacts are written **before** `Validate` runs, so
  validators that inspect the on-disk file (promtool, nginx -t) see the NEW
  content; a validate failure restores the snapshot and never applies
- Drivers declare their product knowledge in one place; nothing else in the
  binary knows the product exists

## Build & run

Requires Go 1.25+ (see `go.mod`).

```sh
# build the binary (all drivers are compiled in)
go build -o opamp-agent ./cmd/opamp-agent

# or install straight to $GOPATH/bin
go install github.com/agardnerIT/opamp-agent/cmd/opamp-agent@latest
```

Run it against your OpAMP server with the `proc` driver and a manifest:

```sh
./opamp-agent \
  -server wss://your-opamp-server.example/v1/opamps \
  -token "$OPAMP_TOKEN" \
  -driver proc \
  -manifest examples/prometheus.yaml
```

| Flag | Meaning |
|---|---|
| `-server` | OpAMP endpoint, `ws(s)://` or `http(s)://` |
| `-token` | agent bearer token (or set `OPAMP_TOKEN`) |
| `-driver` | which compiled-in driver to use (`proc`) |
| `-manifest` | path to the driver manifest (e.g. `examples/prometheus.yaml`) |

The agent persists its identity in an `instance.uid` file (default: the
working directory, override with `-instance-uid-file`) so it keeps the same
agent identity across restarts. Kill it with `SIGINT`/`SIGTERM` for a clean
shutdown.

Other useful commands:

```sh
go test ./...   # run the test suite

go vet ./...    # static checks
```

## Rules for this repo

1. No product names outside a driver package (grep gate).
2. Standard OpAMP only; no server-driven code or driver selection.
3. Every LKG/verify claim must have a capture-style proof behind it.

## Related

- **[opamp-suricata](https://github.com/agardnerIT/opamp-suricata)** — the
  reference vendor driver: Suricata managed by standard OpAMP in three
  files, including live rule reload with per-agent rejection feedback.
  Read its `driver.go` to see the entire product-specific surface.
- `examples/prometheus.yaml` — a complete Tier-1 integration with zero code.
- The vendor note above is expanded in the opamp-server-py planning docs
  (adoption pitch: adopt the bridge vs. implement OpAMP natively).
