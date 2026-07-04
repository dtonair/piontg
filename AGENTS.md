# piontg Architecture Notes

`piontg` is a Go Telegram bot that controls a local Pi coding-agent process via `pi --mode rpc`.

## Common Commands

```bash
go test ./...
go vet ./...
go run ./cmd/piontg --config ./config.example.yaml --dry-run
PIONTG_PI_INTEGRATION=1 go test ./pi -run TestOptionalPiIntegrationGetState -count=1 -v
```

## Project Layout

- `cmd/piontg`: application entrypoint, config loading, signal handling, Telegram polling startup.
- `config`: YAML/env config loading, defaults, validation, path expansion.
- `store`: atomic JSON state persistence (`state.json` plus `.bak`).
- `folders`: allowed-root policy, canonical path checks, safe subfolder discovery, Telegram-safe selection tokens.
- `pi`: Pi RPC subprocess client, strict LF JSONL reader, request/response correlation, event routing, stderr tail capture.
- `session`: single active Pi session manager; owns selected folder/model, Pi lifecycle, state persistence, prompt routing.
- `render`: Telegram-agnostic assistant/tool renderer with message chunking and edit throttling.
- `telegram`: Telegram Bot API adapter, command/callback handlers, event rendering bridge.
- `authz`: single authorized Telegram user guard.

## Security/Design Decisions

- Single-user only: `telegram.allowedUserId` is the sole authorized Telegram user.
- Folder access is root allowlist based. `folders` canonicalizes paths with `filepath.EvalSymlinks` and rejects symlink/path traversal escapes outside configured roots.
- Pi project trust defaults to `--no-approve`; per-root `approve` must be explicit in config.
- Pi tool policy is configurable globally and per root. Empty tool lists mean Pi defaults.
- Pi RPC framing must split only on byte `\n`. Do not replace with generic line readers that split on Unicode separators.
- Telegram rendering should avoid flooding: batch assistant deltas using edit throttling and split messages by rune count.
- Pi extension UI dialog requests are currently cancelled by default in `session` so the RPC process does not block. Rich Telegram UI handling can be added later.

## Testing Notes

- Keep core logic testable with fakes; Telegram and Pi network/process integrations should remain behind small interfaces.
- `pi` has an optional real Pi integration test gated by `PIONTG_PI_INTEGRATION=1`.
- Add tests for any changes to path policy, RPC framing, request correlation, render chunking, or command/callback routing.

## Operational Notes

- Pi provider credentials are expected to be configured outside piontg using normal Pi auth/API key mechanisms.
- `state.json` stores only bot metadata: selected folder/model and Pi session file/id. Pi conversation content remains in Pi agent's default session store; piontg does not configure a custom Pi session directory.
- If the bot crashes during an active Pi turn, the in-flight turn is lost; previous session metadata can still be reused on next startup.
