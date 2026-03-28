# Heartbeat & Reminders

OpenCrow has two scheduling primitives: a **heartbeat** for periodic
awareness and a **reminders** table for one-shot prompts. Both share a
1-minute ticker.

## Heartbeat

Set `OPENCROW_HEARTBEAT_INTERVAL` to a Go duration (e.g. `30m`, `1h`). On
each tick the scheduler reads `<session-dir>/HEARTBEAT.md`, extracts active
checklist items, and sends them to the agent:

```md
# Standing checks
- Check for urgent email
- Review calendar for events in next 2h
- [paused] Weekly report draft
```

Only `- text` lines count. `- [paused] …` is skipped. No other metadata —
obsolete checks are deleted, not marked done. The agent can edit the file
at runtime to add or remove checks.

If the file has no active items, the tick is skipped (no API call). If the
agent replies `HEARTBEAT_OK` the response is suppressed; anything else is
delivered to the conversation.

Heartbeat prompts do not reset the idle timer — if no real user messages
arrive, the pi process is still reaped after the idle timeout.

### Enabling on NixOS

```nix
services.opencrow.settings.OPENCROW_HEARTBEAT_INTERVAL = "30m";
```

The reminder dispatcher runs regardless; this only controls the
HEARTBEAT.md checklist loop.

## Reminders

One-shot reminders live in the `reminders` table in the session's
`opencrow.db`. Enable the bundled `reminders` pi extension to give the
agent structured tools:

- `remind_at(when, prompt)` — schedule a reminder (ISO 8601, normalized to UTC)
- `remind_list()` — list pending reminders
- `remind_cancel(id)` — delete one

Every minute the scheduler runs `DELETE … WHERE fire_at <= now() RETURNING …`
and enqueues each due reminder as a trigger item. Cleanup is atomic — the
agent never manages lifecycle.

`OPENCROW_SESSION_DIR` is exported into pi's environment automatically.

### Enabling on NixOS

```nix
services.opencrow.extensions.reminders = true;
```

This pulls the flake's `extension-reminders` package, which bakes the
`sqlite3` store path into the extension. For non-Nix installs the
extension falls back to PATH lookup, so make sure `sqlite3` is available
there.

## Trigger pipe

External processes (cron jobs, mail watchers, webhooks) can wake the bot
immediately by writing to the session directory's named pipe:

```text
<session-dir>/trigger.pipe
```

Each line written is processed as a separate trigger, delivered
immediately without waiting for a tick.

> [!CAUTION]
> The trigger pipe is an **unauthenticated** input channel. Any process
> that can write to the FIFO can inject arbitrary prompts into `pi`, which
> has full tool access. The FIFO is created with mode `0664`, so any
> process in the `opencrow` group can write to it. Make sure only trusted
> services are members of that group.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `OPENCROW_HEARTBEAT_INTERVAL` | _(empty, disabled)_ | How often to run through HEARTBEAT.md (Go duration) |
| `OPENCROW_HEARTBEAT_PROMPT` | built-in | Preamble sent before the checklist items |
