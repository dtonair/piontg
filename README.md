# piontg

`piontg` is a single-user Telegram bot for controlling a local [Pi coding agent](https://pi.dev/docs/latest) through Pi RPC mode.

It lets the authorized Telegram user:

1. choose a configured project folder,
2. choose an available Pi model,
3. start/continue a Pi RPC session,
4. chat with Pi from Telegram,
5. see streamed assistant text and compact tool status messages.

## Status

Implemented MVP:

- Go Telegram polling bot
- Single Telegram user allowlist
- Configured parent folder roots with safe subfolder discovery
- Pi RPC subprocess client with strict LF JSONL framing
- Persistent local bot state
- Pi session directory support
- Folder/model pickers via inline keyboards
- Assistant streaming renderer with Telegram-safe chunking/edit throttling
- `/start`, `/folder`, `/model`, `/new`, `/abort`, `/status`, `/stop`, `/help`

## Prerequisites

- Go 1.24+
- `pi` installed and available on `PATH`
- Pi provider credentials already configured outside this bot, e.g. through normal Pi auth/API key setup
- A Telegram bot token from [@BotFather](https://t.me/BotFather)
- Your numeric Telegram user ID

## Quick start

```bash
cp config.example.yaml config.yaml
export TELEGRAM_BOT_TOKEN='123456:your-token'
# Edit config.yaml: set telegram.allowedUserId and folders.roots

go run ./cmd/piontg --config ./config.yaml
```

Dry-run config validation:

```bash
go run ./cmd/piontg --config ./config.yaml --dry-run
```

Run tests:

```bash
go test ./...
go vet ./...
```

Optional Pi RPC integration test:

```bash
PIONTG_PI_INTEGRATION=1 go test ./pi -run TestOptionalPiIntegrationGetState -count=1 -v
```

## Configuration

See [`config.example.yaml`](./config.example.yaml).

Important fields:

- `telegram.tokenEnv` / `telegram.token`: Telegram bot token source.
- `telegram.allowedUserId`: the only Telegram user allowed to interact.
- `state.dir`: local state directory for `state.json` and, by default, Pi session files.
- `pi.binary`: Pi executable path, default `pi`.
- `pi.sessionDir`: Pi session file directory.
- `pi.defaultTrust`: default `no-approve`; use `approve` only for explicitly trusted roots.
- `pi.defaultStreamingBehavior`: `follow_up` or `steer` when messages arrive while Pi is running.
- `pi.tools` / `pi.excludeTools`: optional global Pi tool policy.
- `folders.roots`: parent folders whose descendants may be selected from Telegram.

## Telegram commands

- `/start` - show current state and next action
- `/folder` - choose a configured folder/subfolder
- `/model` - choose a Pi model from `get_available_models`
- `/new` - start a new Pi session in the selected folder
- `/abort` - abort the current Pi turn
- `/status` - show folder/model/session/streaming state
- `/stop` - stop the Pi subprocess
- `/help` - show command help

After selecting a folder and model, send a normal Telegram message to prompt Pi.

## Security model

`piontg` is designed for a single trusted operator, not public/group use.

Security controls:

- Only `telegram.allowedUserId` is accepted.
- Folder selection is constrained to canonical descendants of configured roots.
- Symlink escapes and `..` traversal outside roots are rejected.
- Pi project trust defaults to `--no-approve`.
- Tool policy can be restricted globally or per root.
- Pi extension UI dialog requests are cancelled by default so the bot does not hang on unsupported prompts.

Important: Pi can run tools such as shell commands and file edits when enabled. Treat Telegram account and bot token security as equivalent to local terminal access for configured folders.

## Persistence

`piontg` stores lightweight metadata in `<state.dir>/state.json`:

- selected folder
- selected model
- Pi session file/id

Pi conversation history remains in Pi's configured session directory. On restart, piontg lazily starts a new Pi RPC process for the previous folder/session when needed. In-flight turns cannot be resumed exactly after a process crash.

## Troubleshooting

- **No models listed**: configure Pi auth/API keys first using normal Pi setup.
- **Folder missing from picker**: check `folders.maxDepth`, `folders.maxEntries`, and configured roots.
- **Folder rejected**: the canonical path likely resolves outside allowed roots, often due to symlinks.
- **Pi exits immediately**: run with `--dry-run`, check `pi` is on `PATH`, and try `pi --mode rpc --no-session --no-approve` manually in the target folder.
- **Telegram messages stop updating**: Telegram edit limits may be hit; renderer falls back to new messages when edits fail.

## Development layout

- `cmd/piontg`: CLI entrypoint and polling startup
- `config`: YAML/env config loading and validation
- `store`: atomic JSON state persistence
- `folders`: folder allowlist policy and discovery
- `pi`: Pi RPC subprocess client
- `session`: single active Pi session orchestration
- `render`: Telegram-safe rendering/chunking
- `telegram`: Telegram handlers/adapters
- `authz`: single-user authorization
