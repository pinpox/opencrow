# Extensions

Pi supports [extensions](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md)
— TypeScript modules that hook into the agent lifecycle, register custom tools,
and modify behavior. OpenCrow passes extension paths to pi via a generated
`settings.json` in `PI_CODING_AGENT_DIR`.

## NixOS module

The NixOS module provides a declarative `extensions` option. Set a value to
`true` to enable a packaged extension that ships with the opencrow flake, or
pass a path for a custom extension:

```nix
services.opencrow.extensions = {
  memory = true;                     # packaged extension (resolved from flake)
  my-ext = ./extensions/my-ext.ts;   # custom extension
};
```

Setting a value to `false` explicitly disables an extension (useful for
overriding defaults from other modules). The attrset is mergeable across NixOS
module files.

Extra keys for pi's `settings.json` (e.g. `packages`, `compaction`) can be
added via `piSettings`:

```nix
services.opencrow.piSettings = {
  compaction.enabled = true;
};
```

## Packaged extensions

### memory

Cross-session recall using [sediment](https://github.com/rendro/sediment), a
local semantic vector store. Conversations and compaction summaries are stored
automatically; relevant memories are recalled and injected into context before
each prompt. The extension also registers a `memory_search` tool the LLM can
use explicitly.

The sediment binary is patched into the extension at build time — no need to add
it to `extraPackages`. `SEDIMENT_DB` defaults to `/var/lib/opencrow/sediment`
when the extension is enabled.

```nix
services.opencrow.extensions.memory = true;
```

See [`extensions/memory/`](../extensions/memory/) for the source.

## Writing an extension

See the [pi extensions documentation](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md)
for the full API. In short, create a TypeScript file that exports a default
function receiving the `ExtensionAPI`:

```typescript
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("agent_end", async (event) => {
    // react to agent lifecycle events
  });

  pi.registerTool({
    name: "my_tool",
    // register custom tools callable by the LLM
  });
}
```

To package an extension for the NixOS module, add it under `extensions/<name>/`
with an `index.ts` entry point, create a package in `nix/`, and expose it in
`flake.nix` as `extension-<name>`. The module resolves `extensions.<name> = true`
to the corresponding flake package output.
