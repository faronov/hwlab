# Contributing

## Setup

```sh
make build     # binaries into bin/
make test      # go test ./...
make race      # go test -race ./...
make lint      # golangci-lint run (if installed)
make proto     # regenerate pkg/api/pluginv1 (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
```

Go version: see `go.mod`. Run `gofmt -l .` and `go vet ./...` before sending
changes; both must be clean. Use `export HWLAB_HOME=$(mktemp -d)` while
experimenting so your own sessions are untouched.

## Ground rules

1. **The core never mentions a vendor.** Vendor knowledge belongs in a plugin;
   the core talks capabilities. If the core needs something a plugin cannot
   express, extend the protocol instead.
2. **Protocol changes are additive** (`plugin.proto`), keep `ProtocolVersion`
   unless the change is breaking, and are accompanied by an ADR if they alter
   the contract's meaning.
3. **High-rate data is batched** — never one message per sample, and never
   returned raw over MCP.
4. **Timestamps are int64 ns on the session timeline** (see ARCHITECTURE §5).
5. **Dangerous operations are declared** (`dangerous_config_keys`,
   `MethodSpec.danger`) and bounded inside the plugin as well.
6. **Hardware code needs a hardware-free test**: put the wire protocol in a
   pure package (`plugins/ppk2/protocol`) and test it with a simulated port or
   recorded bytes. CI has no hardware.
7. **Do not claim unverified behaviour.** If you implement a protocol from a
   reference, record the source and what is unverified in an ADR.

## Adding a plugin

1. Create `plugins/<id>/` and `plugins/<id>/cmd/hwlab-plugin-<id>/main.go`
   calling `pluginsdk.Serve`. Use `plugins/fake` as a template.
2. Implement `pluginsdk.Plugin`: `Info`, `ListDevices`, `OpenDevice`,
   `StartCapture`, `Invoke`, …. Declare capabilities, methods with danger
   levels and dangerous config keys.
3. Emit batches through the `Sink`; use `pluginsdk.Clock` for timestamps.
4. Test it with `pluginsdk.NewTestSink`; add the binary to the `build` target
   in the `Makefile`.
5. Document it in the README status table and the ROADMAP.

## Tests

- Unit tests live next to the code. Pure packages (`timeline`, `analysis`,
  `correlation`, `session`, `pcap`, `policy`, `experiment`) should stay I/O-free
  or use `t.TempDir()`.
- `internal/app/integration_test.go` is the end-to-end definition-of-done test
  (builds the fake plugin, runs the example experiment, checks files, timeline,
  correlation, plots and replay). Keep it green.
- `internal/plugin` tests cover launch, handshake and crash/relaunch;
  `internal/mcp` tests drive the server through an in-memory transport.

## Commits and reviews

Small, focused changes; explain *why* in the message. Record significant
decisions as ADRs in `docs/adr/` (`NNNN-title.md`: Context, Decision,
Consequences).
