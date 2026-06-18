# Hearth CLI

The host-side companion to [Hearth](https://hearthcmd.com/) — approve AI agent actions from your phone.

`hearth` runs a background daemon on each of your machines. The daemon enrolls the host with the Hearth relay server, launches agents (Claude Code, Codex, Gemini, Copilot, Pi) under its supervision, streams their transcripts to your phone, and forwards every permission request to the iOS app for approval.

## Install

### Prebuilt Binaries

Download from the [releases](https://github.com/HearthCmd/hearth-cmd-cli/releases) page:

| File | Platform |
|------|----------|
| `hearth-darwin-amd64` | macOS (Intel) |
| `hearth-darwin-arm64` | macOS (Apple Silicon) |
| `hearth-linux-amd64` | Linux (x86_64) |
| `hearth-linux-arm64` | Linux (ARM64) |

The Linux binaries also run under WSL2. macOS binaries are codesigned and notarized.

```bash
chmod +x hearth-*
mv hearth-darwin-arm64 /usr/local/bin/hearth   # example for Apple Silicon
```

### Install Script

```bash
curl -sSL https://hearthcmd.com/install.sh | bash
```

### Build from Source

```bash
# Dev (api.hearthcmd.dev — what `scripts/build.sh dev` emits)
go build -ldflags "-X main.version=VERSION -X main.wsURL=wss://api.hearthcmd.dev/ws/relay" -o hearth .

# Prod (api.hearthcmd.com — what `scripts/build.sh prod` emits)
go build -ldflags "-X main.version=VERSION -X main.wsURL=wss://api.hearthcmd.com/ws/relay" -o hearth .
```

Or use `scripts/build.sh`, which auto-detects the version from git tags and cross-compiles for every supported platform.

## Quick Start

```bash
# Register this host under your Hearth account
hearth login you@example.com

# Launch the background daemon
hearth host start

# Spawn a one-off agent in the current directory
hearth hh agent create --temp
```

`--temp` creates a working directory entry rooted at the current shell directory (override with `--wd <path>`), a matching position, and the agent itself — all in one step — then drops you into `hearth talk` so you can start chatting. Repeat invocations in the same directory reuse the wd row; if an agent is already active there you'll be asked whether to sleep it and replace. Open the Hearth iOS app to see the agent, approve its tool calls, and watch its transcript live.

For a perpetual, named agent instead, run `hearth hh agent create` (interactive) or wire the flags directly (see `--help`).

## Commands

```
hearth <command> [flags]
```

| Command | Purpose |
|---------|---------|
| `register <email>` | Enroll this host under a Hearth account. Writes `user_id` and `host_id` to `~/.hearth/config`. |
| `daemon {start,stop,status}` | Manage the per-host background daemon. |
| `hh agent create [--temp] [--wd <path>]` | Create an agent. With `--temp`, creates a disposable agent rooted at the current shell directory (or `--wd`) and execs into `talk`. Re-running in the same directory prompts to sleep and replace any active occupant. |
| `hh {agent,position,wd,user,ai_model,household,host,device,invite,job_description} <list\|get\|create\|update\|delete>` | CRUD on every household entity. |
| `wd create` | Create a bare working directory (without a paired position). |
| `talk [--focus <id>]` | TUI for live transcripts and sending input to active agents. Tab switches focus. |
| `update` | Self-update to the latest release. |
| `version` | Print version and build settings. |

Run any command with `--help` for its flags.

## How It Works

- The daemon holds a single multiplexed WebSocket to the relay server (`/ws/daemon`) and keys every frame by `ai_agent_instance_id`.
- Each agent runs as a child of the daemon inside a PTY, with an interposition library (`DYLD_INSERT_LIBRARIES` / `LD_PRELOAD`) that routes tool-request approvals through the daemon.
- A detached `hearth stream` subprocess per agent tails the agent's transcript JSONL and forwards it to the daemon via a per-agent bridge file; the daemon forwards each line to the relay.
- The host row's `desired_status` column is flipped to `connected` on graceful daemon start and `disconnected` on graceful stop. Crashes and network drops leave it alone — so the server can tell "user turned it off" from "the host fell over".

## Configuration

Settings can be provided via flags, environment variables, or the config file. Priority: flags > env vars > config.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `HEARTH_DAEMON_HOST_ID` | Override the enrolled host_id on daemon start. |
| `HEARTH_LOG` | Log file path (default `/tmp/hearth-<pid>.log`; daemon logs to `~/.hearth/daemon.log`). |

### Config File

`~/.hearth/config` is a key=value file. `register` writes `user_id` and `host_id` there; other keys can be added manually.

## On-Disk State

- `~/.hearth/` — daemon config, log, PID file.
- `/tmp/hearth-daemon.sock` — IPC socket for CLI subcommands.
- `/tmp/hearth-bridge-<ai_agent_instance_id>` — per-agent transcript bridge file.
- `/tmp/hearth-stream-<ai_agent_instance_id>.pid` — PID of the detached transcript streamer.
- `/tmp/.gl-<hex>` + `/tmp/gl-<hex>.sock` — extracted interpose library and its IPC socket.
- `$HOME/hearth_agents/<org_slug>/full_time/<name>/` — default path for full-time agent working directories (suggested in the wizard; user can override at the prompt).
- `$HOME/hearth_agents/<org_slug>/temp/<shortID>/` — server-minted path for iOS one-tap temp agents (CLI `--temp` defaults to the shell's current directory instead).

hearth also writes a one-time trust-dialog acceptance into `~/.claude.json` for each new cwd before launching claude, and installs a `GEMINI.md` / `AGENTS.md` / `.github/copilot-instructions.md` in the agent's cwd depending on the harness.

## Testing

```bash
go test -tags integration -v -timeout 120s ./...
```

The integration tests compile hearth against a local test server and exercise the daemon IPC, the temp-agent flow, transcript streaming, and the graceful-shutdown path.

## Learn More

<https://hearthcmd.com/>

## License

Functional Source License — see `LICENSE.txt`.
