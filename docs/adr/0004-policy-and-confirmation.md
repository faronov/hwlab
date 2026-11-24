# ADR 0004: Policy and confirmation for dangerous operations

Status: accepted

## Context

An AI assistant can call tools autonomously. Applying a supply voltage,
writing flash or sending raw SCPI can damage hardware.

## Decision

- Plugins declare danger: `PluginInfo.dangerous_config_keys` for configuration
  that changes electrical state, and `MethodSpec.danger`
  (`SAFE` / `CONFIRM`) per invokable method.
- The core's `policy.Policy` (default: `RequireConfirmation`) authorises every
  `Action`; unconfirmed dangerous actions fail with `ConfirmationRequired`.
- Confirmation channels: CLI `--yes`; MCP `confirm: true` whose tool
  description tells the model to set it only after explicit user approval.
- `allow_dangerous: true` in an experiment file pre-approves for unattended CLI
  runs only; it is **ignored over MCP** so an assistant cannot approve itself
  through a file it wrote.
- `Invoke` refuses methods the plugin did not declare.
- Plugins must clamp and validate their own parameters regardless of policy.
- Dry runs: a CONFIRM method may mark its boolean `dry_run` argument in its
  args schema with `"x-hwlab-dry-run": true`. A call with `dry_run: true`
  (a JSON boolean) to such a method needs no confirmation; the plugin
  promises that it then only reads and plans (the same footprint as its SAFE
  methods). Unmarked methods need confirmation whatever their arguments,
  and dangerous configuration keys still need it. First used by `esprom`
  program/restore (ADR 0041).

## Consequences

- Confirmation is a convention enforced in the core, not a security boundary
  against a hostile local user.
- The `Policy` interface allows allow-lists, roles and audit logs later.
