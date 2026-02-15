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
