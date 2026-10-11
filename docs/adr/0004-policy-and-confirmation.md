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

## Consequences

- Confirmation is a convention enforced in the core, not a security boundary
  against a hostile local user.
- The `Policy` interface allows allow-lists, roles and audit logs later.
