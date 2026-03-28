/**
 * Reminders Extension — one-shot scheduled prompts for opencrow
 *
 * Gives the LLM structured tools to manage rows in the `reminders` table
 * of opencrow.db. The Go-side scheduler polls that table every minute and
 * delivers due reminders as trigger messages, deleting them atomically.
 *
 * Tools:
 *   remind_at(when, prompt) → id   — schedule a one-shot reminder
 *   remind_list()           → rows — list pending reminders
 *   remind_cancel(id)              — delete a reminder
 *
 * The extension only writes to SQLite; all scheduling, delivery and
 * cleanup is owned by the opencrow process.
 */

import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "@sinclair/typebox";

// Nix build substitutes the store path here. If the placeholder survives
// (non-Nix install), fall back to PATH lookup.
const SQLITE_BIN_RAW = "@@SQLITE_BIN@@";
const SQLITE_BIN = SQLITE_BIN_RAW.startsWith("@@") ? "sqlite3" : SQLITE_BIN_RAW;

const DB_PATH =
  process.env.OPENCROW_SESSION_DIR
    ? `${process.env.OPENCROW_SESSION_DIR}/opencrow.db`
    : undefined;

// Human-readable delta so the agent can sanity-check its own timezone
// math ("in 13h" when the user said "in an hour" is an obvious red flag).
function humanizeDelta(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  if (h < 24) return rm ? `${h}h ${rm}m` : `${h}h`;
  const d = Math.floor(h / 24);
  const rh = h % 24;
  if (d < 7) return rh ? `${d}d ${rh}h` : `${d}d`;
  const w = Math.floor(d / 7);
  const rd = d % 7;
  return rd ? `${w}w ${rd}d` : `${w}w`;
}

// SQLite single-quote escaping: double the quote. Input is always wrapped
// in single quotes, so this is sufficient to prevent injection.
function q(s: string): string {
  return `'${s.replace(/'/g, "''")}'`;
}

// Normalize to ISO 8601 UTC so the scheduler's datetime() comparison
// matches. Rejects timestamps without an explicit offset: Date.parse
// treats "2025-06-15T14:00" as local to the *server*, not the user, and
// "2025-06-15" as UTC midnight — both silently wrong. Forcing the agent
// to be explicit avoids reminders firing hours off.
function normalizeWhen(when: string): string {
  if (!/(?:Z|[+-]\d{2}:?\d{2})$/.test(when.trim())) {
    throw new Error(
      `timestamp '${when}' has no timezone — append Z or an offset, ` +
        `e.g. 2025-06-15T14:00:00Z or 2025-06-15T14:00:00+02:00`,
    );
  }
  const ms = Date.parse(when);
  if (Number.isNaN(ms)) {
    throw new Error(
      `invalid timestamp '${when}' — use ISO 8601, e.g. 2025-06-15T14:00:00Z`,
    );
  }
  // A past timestamp would insert a row that fires on the very next tick
  // — almost certainly a timezone or arithmetic mistake on the agent's
  // part. Reject it here so the agent gets immediate feedback instead of
  // a confusing instant-fire.
  if (ms <= Date.now()) {
    throw new Error(
      `timestamp '${when}' is in the past — reminders must be in the future`,
    );
  }
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

export default function remindersExtension(pi: ExtensionAPI) {
  if (!DB_PATH) {
    // OPENCROW_SESSION_DIR is exported by opencrow's StartPi; if it is
    // missing we are running outside opencrow — silently skip.
    return;
  }

  async function sqlite(sql: string, signal?: AbortSignal): Promise<string> {
    const result = await pi.exec(
      SQLITE_BIN,
      // .timeout mirrors the Go side's busy_timeout(5000): WAL persists
      // in the file header but busy_timeout does not, so without this the
      // CLI fails SQLITE_BUSY instantly when racing the Go dispatcher.
      ["-batch", "-noheader", "-cmd", ".timeout 5000", DB_PATH, sql],
      { signal, timeout: 5000 },
    );
    if (result.code !== 0) {
      throw new Error(`sqlite3 failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout.trim();
  }

  pi.registerTool({
    name: "remind_at",
    label: "Set reminder",
    description:
      "Schedule a one-shot reminder. The prompt is delivered back as a " +
      "trigger message at the given time (±1 min), then auto-deleted.",
    parameters: Type.Object({
      when: Type.String({
        description:
          "Future ISO 8601 timestamp with explicit timezone, " +
          "e.g. 2025-06-15T14:00:00+02:00",
      }),
      prompt: Type.String({
        description: "Message to deliver when the reminder fires.",
      }),
    }),
    async execute(_id, params, signal) {
      const at = normalizeWhen(params.when);
      const delta = Date.parse(at) - Date.now();
      const out = await sqlite(
        `INSERT INTO reminders (fire_at, prompt) VALUES (${q(at)}, ${q(params.prompt)}); ` +
          `SELECT last_insert_rowid();`,
        signal,
      );
      return {
        content: [
          {
            type: "text",
            text: `Reminder #${out} set for ${at} — in ${humanizeDelta(delta)}`,
          },
        ],
        details: { id: Number(out), fire_at: at },
      };
    },
  });

  pi.registerTool({
    name: "remind_list",
    label: "List reminders",
    description: "List pending reminders (id, fire_at, prompt).",
    parameters: Type.Object({}),
    async execute(_id, _params, signal) {
      const out = await sqlite(
        `SELECT id || '  ' || fire_at || '  ' || prompt FROM reminders ORDER BY fire_at;`,
        signal,
      );
      return {
        content: [{ type: "text", text: out || "No reminders pending." }],
        details: {},
      };
    },
  });

  pi.registerTool({
    name: "remind_cancel",
    label: "Cancel reminder",
    description: "Delete a pending reminder by id.",
    parameters: Type.Object({
      id: Type.Integer({ description: "Reminder id to cancel" }),
    }),
    async execute(_id, params, signal) {
      const out = await sqlite(
        `DELETE FROM reminders WHERE id = ${params.id}; SELECT changes();`,
        signal,
      );
      const n = Number(out);
      return {
        content: [
          {
            type: "text",
            text: n > 0 ? `Reminder #${params.id} cancelled.` : `No reminder with id ${params.id}.`,
          },
        ],
        details: { deleted: n },
      };
    },
  });
}
