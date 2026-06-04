# AGENTS.md — ocgo

ocgo is a single-binary Go CLI proxy that lets Claude Code and Codex CLI run against an OpenCode Go subscription. It translates Anthropic Messages API ↔ OpenAI Chat Completions and exposes a lightweight Responses API adapter for Codex.

## Structure

```
cmd/ocgo/main.go        — ~3300-line monolithic CLI (cobra commands, proxy, API translation)
cmd/ocgo/main_test.go   — tests
cmd/ocgo/detach_*.go    — platform-specific process detachment
cmd/ocgo/listener_pid_*.go — platform-specific PID resolution
scripts/release.sh      — GitHub release + Homebrew tap update
```

Everything lives in `cmd/ocgo/`. There are no internal packages. The binary is `ocgo`.

## Key constants (main.go)

| Constant | Value |
|----------|-------|
| `defaultHost` | `127.0.0.1` |
| `defaultPort` | `3456` |
| `openAIURL` | `https://opencode.ai/zen/go/v1/chat/completions` |
| `anthropicURL` | `https://opencode.ai/zen/go/v1/messages` |
| `codexProfileName` | `ocgo-launch` |

## Build & test

```bash
make build          # → bin/ocgo
make test           # → go test ./...
make install        # → ~/go/bin/ocgo
make run            # → go run ./cmd/ocgo
make release TAG=vX.Y.Z  # → GitHub release + brew tap
```

## API endpoints exposed

- `GET /health`
- `POST /v1/messages` — Anthropic → OpenAI translation
- `POST /v1/messages/count_tokens` — returns 0 (stub)
- `POST /v1/chat/completions` — pass-through to OpenCode Go
- `POST /v1/responses` — Responses API → Chat Completions adapter for Codex

## Key design facts

- **Monolithic single file**: All logic is in `main.go`. Platform-specific helpers are tiny shims (`detach_*.go`, `listener_pid_*.go`). Do not split into packages without strong reason.
- **Cobra CLI**: All commands registered in `main()`. Subcommands: `setup`, `list` (aliases: `ls`, `models`), `mapping`, `launch`, `serve`, `stop`, `status`.
- **Model discovery**: Three-tier fallback — official OpenCode Go `/v1/models` → `models.dev` API → hardcoded list. Model metadata enriches Codex catalog generation.
- **Model mapping**: Stored in `~/.config/ocgo/model-mapping.json`. Separate `claude` and `codex` namespaces. Claude supports family-prefix fuzzy matching (e.g., `claude-sonnet` matches `claude-sonnet-4-5`).
- **Codex profile**: Writes `~/.codex/ocgo-launch.config.toml` (newer Codex) and strips legacy sections from `~/.codex/config.toml`. Generates `~/.codex/ocgo-models.json` catalog.
- **Proxy lifecycle**: `launch` starts a managed foreground server (killed on exit). `serve --background` detaches. Status checked via `/health` endpoint.
- **Dependency**: Only `github.com/spf13/cobra`. No other external deps.

## Configuration paths

| File | Purpose |
|------|---------|
| `~/.config/ocgo/config.json` | API key, host, port |
| `~/.config/ocgo/model-mapping.json` | Tool → model mappings |
| `~/.config/ocgo/ocgo.pid` | Background proxy PID |
| `~/.config/ocgo/ocgo.log` | Background proxy logs |
| `~/.codex/ocgo-launch.config.toml` | Codex profile (new) |
| `~/.codex/config.toml` | Codex legacy profile (stripped) |
| `~/.codex/ocgo-models.json` | Codex model catalog |

## Documentation

README.md is the sole user-facing documentation — installation, usage, configuration, development, and release instructions. No separate docs/ directory.
