<h1 align="center">ocgo</h1>
<div align="center">
  <a href="https://github.com/emanuelcasco/ocgo/releases">
    <img alt="GitHub release" src="https://img.shields.io/github/v/release/emanuelcasco/ocgo?color=blue">
  </a>
  <a href="https://github.com/emanuelcasco/ocgo/releases">
    <img alt="GitHub downloads" src="https://img.shields.io/github/downloads/emanuelcasco/ocgo/total">
  </a>
  <a href="https://github.com/emanuelcasco/ocgo/blob/main/LICENSE">
    <img alt="GitHub license" src="https://img.shields.io/github/license/emanuelcasco/ocgo">
  </a>
  <a href="https://go.dev/doc/go1.22">
    <img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/emanuelcasco/ocgo">
  </a>
</div>

<br/>

<div align="center">
 <img alt="Logo" src="assets/ocgo-logo.svg">
</div>

<br/>


<div align="center">
  <a href="https://github.com/emanuelcasco/ocgo">ocgo</a> is a small Go CLI for using your OpenCode Go subscription from Claude Code or Codex CLI in one command — no manual proxy setup required.
  <br/>
  <br/>
  🤖 <em>Claude Code support.</em>  🧠 <em>Codex CLI support.</em> ⚡ <em>Local compatibility proxy.</em>
</div>

## Why `ocgo`?

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
- Map Claude Code or Codex model names to OpenCode Go models when you want transparent model routing.
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

### Map tool models to OpenCode Go models

`ocgo` can route model names used by Claude Code or Codex to OpenCode Go models. Mappings are empty by default; add only the routes you want. The mapping is stored in:

```text
~/.config/ocgo/model-mapping.json
```

Show the current mapping for a tool:

```bash
ocgo mapping claude show
ocgo mapping codex show
```

Get, set, or remove one mapping:

```bash
ocgo mapping claude get claude-sonnet-4-5
ocgo mapping claude set claude-sonnet-4-5 kimi-k2.6
ocgo mapping claude unset claude-sonnet-4-5
ocgo mapping codex set gpt-5 deepseek-v4-pro
ocgo mapping codex rm gpt-5
```

Open the mapping file in `$EDITOR`:

```bash
ocgo mapping claude open
ocgo mapping codex open
```

Exact mappings take precedence. For Claude, family mappings such as `claude-sonnet` can also match versioned names such as `claude-sonnet-4-5` when no exact entry exists.

#### Claude mapping

Claude Code primarily shows Claude/Anthropic models in its `/model` picker. It can show a single custom model in some launch modes, but `ocgo` cannot reliably inject the full OpenCode Go model catalog into Claude Code's picker.

For Claude Code, mappings are useful to make Claude's usual aliases or model names resolve to the OpenCode Go models you prefer, without switching models every session:

```bash
ocgo mapping claude set claude-sonnet kimi-k2.6
ocgo mapping claude set claude-opus deepseek-v4-pro
ocgo mapping claude show
```

With the `claude-sonnet` family mapping above, requests for versioned Sonnet names also route to the same target unless an exact mapping exists:

```text
claude-sonnet-4-5 -> kimi-k2.6
```

When launching Claude, `ocgo` prints the active mapping and exports Claude's default model environment variables only for configured routes.

#### Codex mapping

Codex is different: `ocgo` can already provide a model catalog to Codex through `~/.codex/ocgo-models.json`, so OpenCode Go models can appear directly in Codex's model selection UI.

For Codex, mappings are mostly useful for compatibility with prompts, scripts, tools, or existing config that still request original Codex/OpenAI model names. For example:

```bash
ocgo mapping codex set gpt-5.5 deepseek-v4-pro
```

Then a request for `gpt-5.5` is routed to `deepseek-v4-pro`. Mapped aliases are also included in the generated Codex model catalog so they can appear alongside OpenCode Go model IDs.

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

If Claude Code requests a Claude model name, `ocgo` routes the request through `ocgo mapping claude`. Unmapped model names pass through unchanged. `ocgo launch claude` prints the active mapping before starting Claude Code.

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

When `ocgo launch codex` runs, it writes or updates the `ocgo-launch` Codex profile. For newer Codex versions it writes `~/.codex/ocgo-launch.config.toml`; for compatibility with older Codex versions it also writes legacy profile sections in `~/.codex/config.toml`:

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

Codex model names can be routed through `ocgo mapping codex`. Mapped Codex aliases are included in the generated `~/.codex/ocgo-models.json` catalog so they can appear alongside OpenCode Go models.

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
git clone https://github.com/emanuelcasco/ocgo.git
cd ocgo
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
