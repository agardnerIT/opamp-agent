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
OpAMP Server ── OpAMP Agent (this repo) ── Your product
```

The agent speaks standard OpAMP to the server on the left. On the right, it
talks to your product through one of two contracts — pick the one that fits
(see [Making your product speak OpAMP](#making-your-product-speak-opamp)).

```
driver/driver.go     Interface 2 — the six-verb contract (the vendor-facing surface)
bridge/bridge.go     The commodity layer: opamp-go transport + the LKG
                     render→apply→verify pipeline
drivers/             Compiled-in drivers
  proc/              Manifest-driven driver for config-file + reload products
                     (no product-specific code — a manifest is the whole
                     vendor artifact; see examples/prometheus.yaml)
cmd/opamp-agent/     Binary; drivers are compiled in and selected by flag
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

## Making your product speak OpAMP

This is the part you actually have to write, and it is one of two things:

- **A YAML manifest, zero code** — if your product takes its config from a
  file, can reload it (HTTP endpoint or signal), and exposes a health
  endpoint. The built-in `proc` driver does the rest.
- **A Go driver, ~6 methods** — if your product's management surface is
  anything else (control socket, admin API, bespoke behavior). You implement
  `driver.Driver`; the bridge does everything else.

And the standing exception, restated: if you *can* embed an OpAMP client
library in your agent, do that instead (see the vendor note at the top).
This section is for when you can't or won't.

### Path 1: a manifest, no code

`examples/prometheus.yaml` is a complete integration:

```yaml
driver: proc
identity:
  service_name: prometheus
config:
  path: /opt/homebrew/etc/prometheus.yml
apply:
  method: http_endpoint
  endpoint: http://127.0.0.1:9090/-/reload
validate:
  command: ["/opt/homebrew/bin/promtool", "check", "config", "{config_path}"]
report:
  health_endpoint: http://127.0.0.1:9090/-/healthy
```

Field reference:

| Field | Required | Meaning |
|---|---|---|
| `driver` | yes | must be `proc` |
| `identity.service_name` | yes | reported in AgentDescription |
| `config.path` | yes | the config file the agent owns, renders, and snapshots for LKG recovery |
| `apply.method` | yes | `http_endpoint` or `signal` |
| `apply.endpoint` | if http_endpoint | the reload URL (e.g. `/-/reload`) |
| `apply.signal` + `apply.pid_file` | if signal | `SIGHUP`, `SIGUSR1`, or `SIGUSR2`, plus the path to the pid file |
| `validate.command` | no | fixed argv to run pre-apply; `{config_path}` is replaced with `config.path`. Omit it and the bridge degrades honestly: apply-time `Verify` becomes the only guard |
| `report.health_endpoint` | yes | URL polled for health reporting |

Three properties worth knowing before you trust it: the manifest is a strict
allowlist (an unknown field is a load error, so typos don't silently disable
a check), commands are fixed argv from the manifest — never from the server
— and manifests are selected locally with `-manifest`, never pushed by the
server.

### Path 2: a Go driver

If file-plus-reload doesn't describe your product, implement the interface in
`driver/driver.go`. It is six verbs, none of which know OpAMP exists:

```go
func (d *MyDriver) Identity() Identity              // name, version, OwnedFiles
func (d *MyDriver) Render(cfg []byte) (Rendered, error)  // config → files on disk
func (d *MyDriver) Validate(r Rendered) error        // pre-apply check; ErrUnsupported is honest
func (d *MyDriver) Apply(ctx, r) error               // signal / reload API / socket
func (d *MyDriver) Verify(ctx, r) (applied bool, reason string)  // did it ACTUALLY take effect?
func (d *MyDriver) Health(ctx) Health
func (d *MyDriver) EffectiveConfig(ctx) map[string][]byte
```

Put it in `drivers/<yourproduct>/`, add one line to the switch in
`cmd/opamp-agent/main.go`, rebuild, and run with `-driver <yourproduct>`.

You write only product knowledge. The bridge gives you, once, for all
drivers: the OpAMP transport and enrollment, LKG snapshot-and-rollback of
`OwnedFiles` on failed verification, artifact writes (files land on disk
**before** `Validate` runs, so validators like `promtool` or `nginx -t` see
the new content), and the rule that apply responses are never trusted —
your `Verify` is the only source of truth.

For a worked example of a full driver, see
[opamp-suricata](https://github.com/agardnerIT/opamp-suricata)'s
`driver.go` — the entire product-specific surface of that integration is
three files.

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

The agent persists its identity in an `instance.uid` file in the working
directory, so it keeps the same agent identity across restarts. Run it with
`--list-drivers` to see what's compiled in. Kill it with `SIGINT`/`SIGTERM`
for a clean shutdown.

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
