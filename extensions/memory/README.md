# Memory Extension

Cross-session recall for opencrow using [sediment](https://github.com/rendro/sediment), a local semantic vector store.

## What it does

- **Captures** conversation content and compaction summaries into sediment
- **Recalls** relevant memories before each prompt and injects them into context
- **Provides** a `memory_search` tool the LLM can use to explicitly search past conversations

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
