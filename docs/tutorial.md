# Tutorial: NixOS deployment with Matrix

This walkthrough sets up OpenCrow as a Matrix bot on NixOS using the included
NixOS module. The module runs the bot inside a systemd-nspawn container for
isolation.

## 1. Add the flake inputs

```nix
# flake.nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    opencrow.url = "github:pinpox/opencrow";
    opencrow.inputs.nixpkgs.follows = "nixpkgs";

    # pi coding agent
    llm-agents.url = "github:numtide/llm-agents.nix";
    llm-agents.inputs.nixpkgs.follows = "nixpkgs";
  };
}
```

## 2. Create a Matrix account for the bot

Register a new Matrix account on your homeserver for the bot. You'll need:
- The homeserver URL (e.g. `https://matrix.org`)
- The bot's full user ID (e.g. `@mybot:matrix.org`)
- An access token

To get an access token, log in with the bot account using `curl`:

```bash
curl -s -X POST https://matrix.org/_matrix/client/v3/login \
  -d '{"type":"m.login.password","user":"mybot","password":"..."}' \
  | jq -r '.access_token'
```

Or extract it from an existing Matrix client's session.

## 3. Import the module and configure the bot

```nix
# configuration.nix
{ self, pkgs, ... }:
{
  imports = [
    self.inputs.opencrow.nixosModules.default
  ];

  services.opencrow = {
    enable = true;
    piPackage = self.inputs.llm-agents.packages.${pkgs.stdenv.hostPlatform.system}.pi;

    environment = {
      OPENCROW_MATRIX_HOMESERVER = "https://matrix.org";
      OPENCROW_PI_PROVIDER = "anthropic";
      OPENCROW_PI_MODEL = "claude-sonnet-4-6";

      # Restrict access to specific Matrix users (optional, empty allows all)
      OPENCROW_ALLOWED_USERS = "@alice:matrix.org,@bob:matrix.org";
    };

    # Extra packages available to the agent inside the container
    extraPackages = with pkgs; [
      curl jq ripgrep fd git python3 w3m
    ];
  };
}
```

## 4. Provide secrets

The bot needs a Matrix access token, user ID, and LLM provider credentials.
Pass secrets into the container using environment files — never put secrets in
the Nix store.

Create a file (e.g. `/run/secrets/opencrow-env`) containing:

```
OPENCROW_MATRIX_ACCESS_TOKEN=syt_...
OPENCROW_MATRIX_USER_ID=@mybot:matrix.org
ANTHROPIC_API_KEY=sk-...
```

Then reference it:

```nix
services.opencrow.environmentFiles = [
  /run/secrets/opencrow-env
];
```

**Or use OAuth instead of an API key** — if you have a Claude Pro/Max
subscription, skip `ANTHROPIC_API_KEY` and authenticate interactively after
deployment:

```bash
sudo opencrow-pi auth login
```

Pi prints a URL — open it in any browser, log in, and paste the redirect URL
back into the terminal.

## 5. Deploy and verify

After deploying your NixOS configuration:

```bash
# Check the container is running
machinectl list

# Follow the bot logs
journalctl -M opencrow -u opencrow -f

# Interactive pi shell inside the container (requires root)
sudo opencrow-pi
```

Invite the bot to a Matrix room or send it a DM. The bot should respond.

## 6. Optional: add skills and extensions

Skills teach the agent new capabilities. Extensions hook into the agent
lifecycle:

```nix
services.opencrow = {
  skills = {
    # Built-in web browsing skill (included by default)
    web = "${pkgs.opencrow}/share/opencrow/skills/web";
    # Custom skill from a local directory
    my-skill = ./skills/my-skill;
  };

  extensions = {
    # Cross-session memory via semantic search
    memory = true;
  };
};
```

See [Skills](skills.md) and [Extensions](extensions.md) for details.

## 7. Optional: customize the personality

Create a `SOUL.md` file that defines the bot's personality and point to it:

```nix
services.opencrow.environment.OPENCROW_SOUL_FILE = "${./soul.md}";
```

```markdown
# SOUL.md — Who You Are

## Identity
- **Name:** My Bot
- **Vibe:** Helpful, concise, technically competent

## Personality
Be genuinely helpful. Prefer action over questions. Use the tools available
to you — read files, run commands, search the web — before asking the user.

## Available Tools
Beyond the basics: curl, jq, ripgrep, fd, git, python3, w3m
```

## Using Nostr instead

To use Nostr as the messaging backend, swap the Matrix environment variables
for Nostr ones:

```nix
services.opencrow.environment = {
  OPENCROW_BACKEND = "nostr";
  OPENCROW_NOSTR_RELAYS = "wss://nos.lol,wss://relay.damus.io";
  OPENCROW_NOSTR_DM_RELAYS = "wss://nos.lol,wss://relay.damus.io";
  OPENCROW_NOSTR_BLOSSOM_SERVERS = "https://blossom.nostr.build";
  OPENCROW_NOSTR_PRIVATE_KEY_FILE = "%d/nostr-private-key";

  # Profile metadata (NIP-01 kind 0)
  OPENCROW_NOSTR_NAME = "mybot";
  OPENCROW_NOSTR_DISPLAY_NAME = "My Bot";
  OPENCROW_NOSTR_ABOUT = "An AI assistant powered by OpenCrow";

  # Restrict access (optional)
  OPENCROW_NOSTR_ALLOWED_USERS = "npub1...";
};

# Pass the private key via credential files
services.opencrow.credentialFiles."nostr-private-key" = /run/secrets/nostr-private-key;
```

See [Configuration](configuration.md) for the full reference.
