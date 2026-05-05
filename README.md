# ocgo

`ocgo` is a small Go CLI that lets [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and [Codex CLI](https://developers.openai.com/codex/cli/) run against an OpenCode Go subscription. It starts a local compatibility proxy, translates Claude Code's Anthropic Messages API requests when needed, exposes OpenAI-compatible endpoints for Codex, and launches tools with the right configuration.

```bash
# 1. Setup your OpenCode API key
ocgo setup

# 2. Start coding!
ocgo launch claude --model kimi-k2.6
ocgo launch codex --model kimi-k2.6
```

Use your OpenCode Go subscription from Claude Code or Codex CLI in one command — no manual proxy setup required.

## Features

- Save and reuse your OpenCode Go API key.
- List known OpenCode Go model IDs.
- Run Claude Code through OpenCode Go with one command.
- Run Codex CLI through OpenCode Go with one command.
- Start, stop, and inspect a local proxy server.
- Exposes Anthropic-compatible and OpenAI-compatible local API layers.
- Supports streaming text responses and basic tool-call translation.

## Requirements

- Go 1.22 or newer.
- A valid OpenCode Go API key.
- Claude Code or Codex CLI installed and available.

## Installation

Install with Homebrew:

```bash
brew install emanuelcasco/tap/ocgo
```

Or tap the repository first:

```bash
brew tap emanuelcasco/tap
brew install ocgo
```

Build from source:

```bash
git clone https://github.com/emanuelcasco/ocgo.git
cd ocgo
make install
```

## Configuration

Run setup and paste your OpenCode Go API key when prompted:

```bash
ocgo setup
```

Or pass the key directly:

```bash
ocgo setup --api-key sk-opencode-your-key
```

Configuration is saved to:

```text
~/.config/ocgo/config.json
```

You can also provide the key at runtime with an environment variable:

```bash
export OCGO_API_KEY=sk-opencode-your-key
```

By default, the local proxy listens on `127.0.0.1:3456`.

## Usage

### List available models

```bash
ocgo list
```

Aliases are also available:

```bash
ocgo ls
ocgo models
```

### Launch Claude Code

Start Claude Code through the local proxy:

```bash
ocgo launch claude
```

Use a specific OpenCode Go model:

```bash
ocgo launch claude --model kimi-k2.6
```

Pass arguments through to Claude Code after `--`:

```bash
ocgo launch claude --model kimi-k2.6 -- -p "How does this repository work?"
```

Allow Claude Code to skip permission prompts:

```bash
ocgo launch claude --yes
```

When `ocgo launch claude` starts Claude Code, it sets:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:3456
ANTHROPIC_AUTH_TOKEN=unused
```

When `--model` is provided, it also sets:

```bash
ANTHROPIC_MODEL=<model>
ANTHROPIC_SMALL_FAST_MODEL=<model>
```

If Claude Code requests a Claude model name or does not provide a model, `ocgo` defaults the upstream OpenCode Go model to `kimi-k2.6`.

### Launch Codex CLI

Start Codex CLI through the local proxy:

```bash
ocgo launch codex
```

Use a specific OpenCode Go model:

```bash
ocgo launch codex --model kimi-k2.6
```

Pass arguments through to Codex after `--`:

```bash
ocgo launch codex --model kimi-k2.6 -- --sandbox workspace-write
```

Configure Codex without launching it:

```bash
ocgo launch codex --config
```

When `ocgo launch codex` runs, it writes or updates this profile in `~/.codex/config.toml`:

```toml
[profiles.ocgo-launch]
openai_base_url = "http://127.0.0.1:3456/v1/"
forced_login_method = "api"
model_provider = "ocgo-launch"
model_catalog_json = "/Users/you/.codex/ocgo-models.json"

[model_providers.ocgo-launch]
name = "OpenCode Go"
base_url = "http://127.0.0.1:3456/v1/"
wire_api = "responses"
```

It then launches:

```bash
codex --profile ocgo-launch -m <model>
```

The Codex process receives `OPENAI_API_KEY=ocgo`; the local proxy injects your real OpenCode Go API key upstream. `ocgo` also writes `~/.codex/ocgo-models.json` so Codex has metadata for OpenCode Go model IDs such as `deepseek-v4-pro`.

## Proxy commands

Run the proxy in the foreground:

```bash
ocgo serve
```

Run it in the background:

```bash
ocgo serve --background
# or
ocgo serve -b
```

Check whether the proxy is running:

```bash
ocgo status
```

Stop the background proxy:

```bash
ocgo stop
```

Proxy runtime files are stored in:

```text
~/.config/ocgo/ocgo.pid
~/.config/ocgo/ocgo.log
```

## Development

### Set up a local development environment

Clone the repository and enter the project directory:

```bash
git clone <repository-url>
cd ocgo-cc
```

Install Go 1.22 or newer, then download dependencies:

```bash
go mod download
```

Build the binary:

```bash
make build
```

The binary is written to:

```text
bin/ocgo
```

Optionally install it to `~/go/bin`:

```bash
make install
```

Make sure the install location is in your `PATH`:

```bash
export PATH="$HOME/go/bin:$PATH"
```

Configure an OpenCode Go API key for local testing:

```bash
bin/ocgo setup
# or, if installed:
ocgo setup
```

Run the CLI without building:

```bash
make run
```

Run tests:

```bash
make test
```

Remove built binaries:

```bash
make clean
```

## Release

This project includes a plain Bash release script, no GoReleaser required. It uses the GitHub CLI to create the GitHub release and update a Homebrew tap formula.

Requirements:

```bash
brew install gh
gh auth login
```

Release a new version:

```bash
make release TAG=v0.1.0
```

By default, releases are published to `emanuelcasco/ocgo` and the Homebrew formula is pushed to `emanuelcasco/homebrew-tap`. You can override those with `GITHUB_REPOSITORY=owner/repo` and `HOMEBREW_TAP_REPO=owner/homebrew-tap`.

The script builds macOS/Linux `amd64` and `arm64` archives, uploads them to GitHub Releases, and commits `Formula/ocgo.rb` to the tap repo.

## How it works

`ocgo` exposes a local compatibility API used by Claude Code and Codex CLI:

- `GET /health`
- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- `POST /v1/chat/completions`
- `POST /v1/responses`

Requests sent to `/v1/messages` are converted from Anthropic Messages format into OpenAI-compatible chat completion requests.

Requests sent to `/v1/chat/completions` are passed through as OpenAI-compatible chat completion requests while `ocgo` injects the configured OpenCode Go API key.

Requests sent to `/v1/responses` use a lightweight OpenAI Responses API adapter for Codex CLI. The adapter converts common Responses input, tool definitions, and streaming text events to and from chat completions.

All upstream requests are forwarded to:

```text
https://opencode.ai/zen/go/v1/chat/completions
```

Claude Code responses are converted back into Anthropic-compatible responses. Codex responses are returned in OpenAI-compatible Chat Completions or Responses API shapes depending on the requested endpoint.

## Limitations

`ocgo` is intentionally lightweight. Token counting currently returns `0`, and Anthropic/OpenAI compatibility is focused on the request and response shapes needed by Claude Code and Codex CLI rather than full API parity. The `/v1/responses` adapter is minimal and targets text/tool workflows used by Codex; it is not a complete OpenAI Responses API implementation.

## License

MIT. See [LICENSE](LICENSE).
