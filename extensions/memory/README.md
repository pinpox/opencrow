# Memory Extension

Cross-session recall for opencrow using [sediment](https://github.com/rendro/sediment), a local semantic vector store.

## What it does

- **Captures** durable facts (`[kind] subject: body`) extracted from
  each turn, plus compaction summaries
- **Recalls** relevant memories before each prompt and injects them into context
- **Provides** a `memory_search` tool for explicit lookup

Facts supersede by subject, so stale exemplars get replaced rather
than accumulating. See `index.ts` for details.

## Requirements

- `sediment` binary on PATH
- `SEDIMENT_DB` environment variable pointing to the database directory

## NixOS module usage

The opencrow NixOS module provides this as a packaged extension:

```nix
services.opencrow = {
  enable = true;
  extensions.memory = true;
  # ...
};
```

Or reference the package directly:

```nix
services.opencrow.extensions.memory =
  opencrow.packages.${system}.extension-memory;
```
