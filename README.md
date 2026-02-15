<p align="center">
  <img src="logo.png" width="200" alt="OpenCrow logo">
</p>

# OpenCrow

A saner alternative to [OpenClaw](https://github.com/openclaw/openclaw).

OpenCrow is a Matrix bot that bridges chat messages to
[pi](https://github.com/badlogic/pi-mono), a coding agent with built-in tools,
session persistence, auto-compaction, and multi-provider LLM support. Instead of
reimplementing all of that in Go, OpenCrow spawns pi as a long-lived subprocess
per room via its RPC protocol and acts as a thin bridge.

```
Matrix Room A --> Bot (Go) --> pi --mode rpc (Room A session)
Matrix Room B -->          --> pi --mode rpc (Room B session)
```

Each room gets its own pi process with an isolated session directory. The Go bot
receives Matrix messages, forwards them to the room's pi process, collects the
response, and sends it back.

## No safety guarantees

There is no whitelisting, permission system, or tool filtering. Trying to bolt
that onto LLM tool use is inherently futile -- the model will find a way around
it. The only real protection is running OpenCrow in a containerized or sandboxed
environment. If you try this out, **do so in a NixOS container, VM, or similar
isolation**. The included NixOS module and clan service do exactly that. Don't
run it on a machine where you'd mind the LLM running arbitrary commands.

## Authentication

Pi needs credentials for your LLM provider. There are two ways to set this up:

**Option A: Environment variable** -- set `ANTHROPIC_API_KEY` (or the
equivalent for your provider) in an environment file and pass it via the
`environmentFiles` option in the clan service.

**Option B: OAuth (Claude Pro/Max)** -- pi supports OAuth against your Anthropic
account, so you can use your subscription instead of API credits. The initial
login is interactive and needs a browser, but subsequent token refreshes happen
automatically.

To set it up, either run pi on your host machine or log into the container
directly:

```
# Option 1: run pi on the host, then copy the auth file
pi
# type /login, complete OAuth in browser
cp ~/.pi/agent/auth.json /var/lib/opencrow/pi-agent/auth.json

# Option 2: log into the container and run pi there
nixos-container root-login opencrow
pi
# type /login, copy the URL it prints into your browser manually
```

The refresh token persists across restarts -- you only need to do this once
(unless the token gets revoked).

## Skills

Pi supports skills -- markdown files that extend the agent's capabilities by
providing instructions and examples for specific tasks. Each skill is a directory
containing a `SKILL.md` file with a YAML frontmatter (`name`, `description`) and
the skill's instructions.

OpenCrow ships with a `web` skill (for browsing with curl/lynx) and passes it to
pi by default. To configure which skills are loaded, set `OPENCROW_PI_SKILLS` to
a comma-separated list of skill directory paths:

```
OPENCROW_PI_SKILLS=/var/lib/opencrow/skills/web,/path/to/custom-skill
```

Each path should point to a directory containing a `SKILL.md` file. These are
passed to pi via `--skill` flags.

To write your own skill, create a directory with a `SKILL.md`:

```markdown
---
name: My Skill
description: What this skill does
---

Instructions for the agent on how to use this skill...
```

When using the NixOS module, the default skill path points to the packaged
`web` skill at `${cfg.package}/share/opencrow/skills/web`. Set
`OPENCROW_PI_SKILLS` in the `environment` option to override or add more.

## Heartbeat

OpenCrow can periodically wake up and check a per-room `HEARTBEAT.md` task list,
prompting the AI proactively if something needs attention. This is disabled by
default.

Set `OPENCROW_HEARTBEAT_INTERVAL` to a Go duration (e.g. `30m`, `1h`) to enable
it. Every minute the scheduler checks each room with a live pi process. If the
interval has elapsed since the last heartbeat, it reads
`<session-dir>/<room-id>/HEARTBEAT.md`. If the file is missing or contains only
empty headers and list items, the heartbeat is skipped (no API call). Otherwise
the file contents are sent to pi with a prompt asking it to follow any tasks
listed there. If pi responds with `HEARTBEAT_OK`, the response is suppressed.
Anything else is delivered to the Matrix room.

Heartbeat prompts do not reset the idle timer -- if no real user messages arrive,
the pi process is still reaped after the idle timeout.

### Trigger files

External processes (cron jobs, mail watchers, webhooks) can wake the bot
immediately by dropping a trigger file:

```
echo "New email from alice@example.com" > /var/lib/opencrow/triggers/'!roomid:matrix.org.trigger'
```

The scheduler picks up `<room-id>.trigger` files on its next tick, passes the
file content as extra context to the heartbeat prompt, and deletes the file. For
atomic writes, write to a `.tmp` file first and rename.

The trigger directory defaults to `<working-dir>/triggers` and can be overridden
with `OPENCROW_HEARTBEAT_TRIGGER_DIR`.

### Configuration

| Variable | Default | Description |
|---|---|---|
| `OPENCROW_HEARTBEAT_INTERVAL` | _(empty, disabled)_ | How often to run heartbeats per room (Go duration) |
| `OPENCROW_HEARTBEAT_PROMPT` | built-in | Custom prompt sent with the HEARTBEAT.md contents |
| `OPENCROW_HEARTBEAT_TRIGGER_DIR` | `<working-dir>/triggers` | Directory for trigger files |
