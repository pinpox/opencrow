# Heartbeat

OpenCrow can periodically wake up and check `HEARTBEAT.md` in the session
directory, prompting the AI proactively if something needs attention. This is
disabled by default.

Set `OPENCROW_HEARTBEAT_INTERVAL` to a Go duration (e.g. `30m`, `1h`) to enable
it. Every minute the scheduler checks whether the heartbeat interval has elapsed
and reads `<session-dir>/HEARTBEAT.md`. If the file is missing or contains only
empty headers and list items, the heartbeat is skipped (no API call). Otherwise
the file contents are sent to pi with a prompt asking it to follow any tasks
listed there. If pi responds with `HEARTBEAT_OK`, the response is suppressed.
Anything else is delivered to the conversation.

Heartbeat prompts do not reset the idle timer — if no real user messages arrive,
the pi process is still reaped after the idle timeout.

## Trigger pipes

External processes (cron jobs, mail watchers, webhooks) can wake the bot
immediately by writing to the session directory's named pipe (FIFO):

```
<session-dir>/trigger.pipe
```

The `trigger.pipe` is created automatically when the session directory
is set up. A dedicated goroutine reads from the pipe and delivers the content
to pi immediately — no waiting for the heartbeat tick.

Example:

```
echo "New email from alice@example.com" > /var/lib/opencrow/sessions/trigger.pipe
```

Each line written to the pipe is processed as a separate trigger.

> [!CAUTION]
> The trigger pipe is an **unauthenticated** input channel. Any process that can
> write to the FIFO can inject arbitrary prompts into `pi`, which has full tool
> access (shell commands, file I/O, network). This is by design — the pipe is
> meant for trusted local automation (cron, webhooks, mail watchers). The FIFO
> is created with mode `0664`, so any process in the `opencrow` group can write
> to it. Make sure only trusted services are members of that group.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `OPENCROW_HEARTBEAT_INTERVAL` | _(empty, disabled)_ | How often to run heartbeats (Go duration) |
| `OPENCROW_HEARTBEAT_PROMPT` | built-in | Custom prompt sent with the HEARTBEAT.md contents |
