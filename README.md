# ocgo

`ocgo` is a small Go CLI that lets [Claude Code](https://docs.anthropic.com/en/docs/claude-code) run against an OpenCode Go subscription. It starts a local Anthropic-compatible proxy, translates Claude Code's Anthropic Messages API requests to OpenCode Go's OpenAI-compatible chat completions endpoint, and launches `claude` with the right environment variables.

```bash
ocgo setup
ocgo launch claude --model kimi-k2.6
```

Use your OpenCode Go subscription from Claude Code in one command — no manual proxy setup required.

## Features

- Save and reuse your OpenCode Go API key.
- List known OpenCode Go model IDs.
- Run Claude Code through OpenCode Go with one command.
- Start, stop, and inspect a local proxy server.
- Supports streaming text responses and basic tool-call translation.

## Requirements

- Go 1.22 or newer.
- A valid OpenCode Go API key.
- Claude Code installed and available as `claude` in your `PATH` when using `ocgo launch claude`.

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

`ocgo` exposes a local subset of the Anthropic API used by Claude Code:

- `GET /health`
- `POST /v1/messages`
- `POST /v1/messages/count_tokens`

Requests sent to `/v1/messages` are converted into OpenAI-compatible chat completion requests and forwarded to:

```text
https://opencode.ai/zen/go/v1/chat/completions
```

Responses are converted back into Anthropic-compatible responses for Claude Code.

## Limitations

`ocgo` is intentionally lightweight. Token counting currently returns `0`, and Anthropic/OpenAI compatibility is focused on the request and response shapes needed by Claude Code rather than full API parity.

## License

MIT. See [LICENSE](LICENSE).
