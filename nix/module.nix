{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.opencrow;
  opencrowPkg = cfg.package;

  # Assemble all declared skills into a single directory.
  # Each skill is symlinked by its name, producing a flat
  # layout that OPENCROW_PI_SKILLS_DIR can scan.
  skillsDir = pkgs.linkFarm "opencrow-skills" (
    lib.mapAttrsToList (name: path: { inherit name path; }) cfg.skills
  );

  # Resolve extension values: `true` means use the corresponding
  # extension package from the opencrow flake, a path is used as-is.
  resolvedExtensions = lib.mapAttrs (
    name: value:
    if value == true then
      self.packages.${pkgs.hostPlatform.system}."extension-${name}"
    else
      value
  ) (lib.filterAttrs (_: v: v != false) cfg.extensions);

  # Generate a settings.json for pi that lists declared extensions.
  # Installed into PI_CODING_AGENT_DIR at service startup so pi
  # auto-discovers them.
  piSettingsJson = pkgs.writeText "pi-settings.json" (
    builtins.toJSON ({ extensions = lib.attrValues resolvedExtensions; } // cfg.piSettings)
  );

  # Host-side wrapper to interact with pi inside the container as the opencrow user.
  opencrowPi = pkgs.writeShellScriptBin "opencrow-pi" ''
    exec machinectl shell opencrow@opencrow \
      /run/current-system/sw/bin/env \
        HOME=/var/lib/opencrow \
        PI_CODING_AGENT_DIR=${cfg.environment.PI_CODING_AGENT_DIR} \
      ${lib.getExe cfg.piPackage} "$@"
  '';
in
{
  options.services.opencrow = {
    enable = lib.mkEnableOption "OpenCrow messaging bot";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.hostPlatform.system}.opencrow;
      defaultText = lib.literalExpression "opencrow.packages.\${system}.opencrow";
      description = "The opencrow package to use.";
    };

    piPackage = lib.mkOption {
      type = lib.types.package;
      description = "The pi coding agent package. Required — typically from llm-agents.nix or similar.";
      example = lib.literalExpression "llm-agents.packages.\${system}.pi";
    };

    signalCliPackage = lib.mkOption {
      type = lib.types.package;
      default = pkgs.signal-cli;
      defaultText = lib.literalExpression "pkgs.signal-cli";
      description = "The signal-cli package to use when OPENCROW_BACKEND is signal.";
    };

    skills = lib.mkOption {
      type = lib.types.attrsOf lib.types.path;
      default = {
        web = "${opencrowPkg}/share/opencrow/skills/web";
      };
      defaultText = lib.literalExpression ''{ web = "''${opencrowPkg}/share/opencrow/skills/web"; }'';
      description = ''
        Skill directories to make available to pi, keyed by name. Each
        value must be a path to a directory containing a SKILL.md file.
        All skills are assembled into a single directory and passed via
        OPENCROW_PI_SKILLS_DIR.
      '';
      example = lib.literalExpression ''
        {
          web = "''${opencrowPkg}/share/opencrow/skills/web";
          my-custom-skill = ./my-custom-skill;
          kagi-search = "''${pkgs.fetchFromGitHub { owner = "someone"; repo = "pi-skills"; rev = "main"; hash = "..."; }}/kagi-search";
        }
      '';
    };

    extensions = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.either lib.types.bool lib.types.path
      );
      default = { };
      description = ''
        Pi extension files or directories to make available, keyed by
        name. Each value can be:
        - `true` to enable a packaged extension shipped with opencrow
          (resolved from the flake's `extension-''${name}` package output)
        - `false` to explicitly disable an extension
        - A path to a .ts file or directory containing an index.ts

        All enabled extensions are written into a generated settings.json
        that pi reads from PI_CODING_AGENT_DIR.

        Bundled extensions: `memory` (cross-session recall via sediment).
      '';
      example = lib.literalExpression ''
        {
          # Enable the bundled memory extension
          memory = true;
          # Custom extension from a local path
          my-ext = ./extensions/my-ext.ts;
        }
      '';
    };

    piSettings = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      description = ''
        Extra keys to include in the generated pi settings.json.
        The `extensions` key is automatically populated from the
        `extensions` option and should not be set here.
      '';
      example = lib.literalExpression ''
        {
          packages = [ "npm:@foo/bar@1.0.0" ];
          compaction = { enabled = true; };
        }
      '';
    };

    environmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.path;
      default = [ ];
      description = ''
        List of environment files containing secrets (on the host).
        Bind-mounted read-only into the container.
        Must define at minimum (across all files):
        - For Matrix: OPENCROW_MATRIX_ACCESS_TOKEN
        - For Nostr: OPENCROW_NOSTR_PRIVATE_KEY or OPENCROW_NOSTR_PRIVATE_KEY_FILE
        - ANTHROPIC_API_KEY (or the appropriate key for your provider)
      '';
    };

    credentialFiles = lib.mkOption {
      type = lib.types.attrsOf lib.types.path;
      default = { };
      description = ''
        Credential files to pass into the container via systemd-nspawn's
        --load-credential. Keys are credential names, values are host paths.
        Inside the container, the opencrow service imports them via
        ImportCredential and they are available under
        $CREDENTIALS_DIRECTORY/<name>.
      '';
      example = lib.literalExpression ''
        { "nostr-private-key" = config.clan.core.vars.generators.opencrow.files.nostr-private-key.path; }
      '';
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = "Extra packages available inside the container and on the service PATH.";
      example = lib.literalExpression "[ pkgs.curl pkgs.jq ]";
    };

    extraBindMounts = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            hostPath = lib.mkOption { type = lib.types.str; };
            isReadOnly = lib.mkOption {
              type = lib.types.bool;
              default = false;
            };
          };
        }
      );
      default = { };
      description = "Additional bind mounts into the container.";
    };

    environment = lib.mkOption {
      type = lib.types.submodule {
        freeformType = lib.types.attrsOf lib.types.str;

        options = {
          OPENCROW_BACKEND = lib.mkOption {
            type = lib.types.enum [
              "matrix"
              "nostr"
              "signal"
            ];
            default = "matrix";
            description = "Messaging backend to use.";
          };

          OPENCROW_MATRIX_HOMESERVER = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Matrix homeserver URL. Required when backend is matrix.";
            example = "https://matrix.example.com";
          };

          OPENCROW_MATRIX_DEVICE_ID = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Matrix device ID.";
          };

          OPENCROW_SIGNAL_ACCOUNT = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Signal account identifier for signal-cli. Required when backend is signal.";
            example = "+12025550123";
          };

          OPENCROW_SIGNAL_CLI_BINARY = lib.mkOption {
            type = lib.types.str;
            default = lib.getExe cfg.signalCliPackage;
            defaultText = lib.literalExpression "lib.getExe config.services.opencrow.signalCliPackage";
            description = "Path to the signal-cli binary.";
          };

          OPENCROW_SIGNAL_CONFIG_DIR = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/opencrow/signal-cli";
            description = "Directory where signal-cli stores account data and downloaded attachments.";
          };

          OPENCROW_SIGNAL_SOCKET_PATH = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/opencrow/signal-cli/opencrow-jsonrpc.sock";
            description = "Unix socket path used by signal-cli daemon JSON-RPC.";
          };

          OPENCROW_NOSTR_RELAYS = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Comma-separated Nostr relay WebSocket URLs. Required when backend is nostr.";
            example = "wss://relay.damus.io,wss://nos.lol";
          };

          OPENCROW_NOSTR_DM_RELAYS = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = ''
              Comma-separated relay URLs to advertise in the bot's NIP-17 DM relay
              list (kind 10050). Only list relays that accept kind 1059 gift wraps.
              If empty, falls back to OPENCROW_NOSTR_RELAYS.
            '';
            example = "wss://relay.damus.io,wss://nos.lol";
          };

          OPENCROW_NOSTR_PRIVATE_KEY_FILE = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Path to file containing Nostr private key (hex or nsec). Required when backend is nostr (unless key is in environment file).";
          };

          OPENCROW_NOSTR_BLOSSOM_SERVERS = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Comma-separated Blossom server URLs for file uploads.";
            example = "https://blossom.nostr.build";
          };

          OPENCROW_NOSTR_ALLOWED_USERS = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Comma-separated npubs or hex pubkeys allowed to interact with the bot. Empty allows all.";
          };

          OPENCROW_NOSTR_NAME = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Bot profile name (NIP-01 kind 0 'name' field).";
          };

          OPENCROW_NOSTR_DISPLAY_NAME = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Bot profile display name (NIP-01 kind 0 'display_name' field).";
          };

          OPENCROW_NOSTR_ABOUT = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Bot profile about/bio (NIP-01 kind 0 'about' field).";
          };

          OPENCROW_NOSTR_PICTURE = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Bot profile picture URL (NIP-01 kind 0 'picture' field).";
          };

          OPENCROW_PI_PROVIDER = lib.mkOption {
            type = lib.types.str;
            default = "anthropic";
            description = "LLM provider for pi (anthropic, openai, google, etc.).";
          };

          OPENCROW_PI_MODEL = lib.mkOption {
            type = lib.types.str;
            default = "claude-opus-4-6";
            description = "Model ID for pi to use.";
          };

          OPENCROW_PI_SESSION_DIR = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/opencrow/sessions";
            description = "Directory for pi session storage.";
          };

          OPENCROW_PI_IDLE_TIMEOUT = lib.mkOption {
            type = lib.types.str;
            default = "30m";
            description = "Idle timeout for pi processes (Go duration format, e.g. 30m, 1h).";
          };

          OPENCROW_PI_WORKING_DIR = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/opencrow";
            description = "Working directory for pi subprocesses.";
          };

          OPENCROW_PI_SYSTEM_PROMPT = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Custom system prompt appended to pi. Empty uses the built-in default.";
          };

          OPENCROW_PI_SKILLS = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Comma-separated list of additional skill paths to pass to pi via --skill. Prefer using the top-level `skills` option instead.";
          };

          OPENCROW_PI_SKILLS_DIR = lib.mkOption {
            type = lib.types.str;
            default = toString skillsDir;
            defaultText = lib.literalExpression ''"''${skillsDir}"'';
            description = "Directory to scan for skill subdirectories (each must contain SKILL.md). Automatically populated from the `skills` option.";
          };

          OPENCROW_SOUL_FILE = lib.mkOption {
            type = lib.types.str;
            default = "${opencrowPkg}/share/opencrow/SOUL.md";
            description = "Path to SOUL.md personality file.";
          };

          PI_CODING_AGENT_DIR = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/opencrow/pi-agent";
            description = "Directory where pi stores its agent configuration and data.";
          };

          OPENCROW_LOG_LEVEL = lib.mkOption {
            type = lib.types.enum [
              "debug"
              "info"
              "warn"
              "error"
            ];
            default = "info";
            description = "Log verbosity. Set to 'debug' to log full conversation content.";
          };

          OPENCROW_HEARTBEAT_INTERVAL = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Heartbeat interval (Go duration, e.g. '30m'). Empty disables heartbeat.";
          };
        };
      };
      default = { };
      description = ''
        Environment variables passed to the opencrow service.
        Known options have defaults and descriptions. Extra variables
        (e.g. provider-specific settings) can be added freely.
      '';
    };
  };

  config = lib.mkIf cfg.enable {

    assertions = [
      {
        assertion =
          cfg.environment.OPENCROW_BACKEND != "matrix" || cfg.environment.OPENCROW_MATRIX_HOMESERVER != "";
        message = "OPENCROW_MATRIX_HOMESERVER is required when OPENCROW_BACKEND is matrix.";
      }
      {
        assertion =
          cfg.environment.OPENCROW_BACKEND != "signal" || cfg.environment.OPENCROW_SIGNAL_ACCOUNT != "";
        message = "OPENCROW_SIGNAL_ACCOUNT is required when OPENCROW_BACKEND is signal.";
      }
      {
        assertion =
          cfg.environment.OPENCROW_BACKEND != "nostr" || cfg.environment.OPENCROW_NOSTR_RELAYS != "";
        message = "OPENCROW_NOSTR_RELAYS is required when OPENCROW_BACKEND is nostr.";
      }
      {
        assertion =
          cfg.environment.OPENCROW_BACKEND != "nostr"
          || cfg.environment.OPENCROW_NOSTR_PRIVATE_KEY_FILE != ""
          # Key may also be provided via environmentFiles or credentialFiles
          || (builtins.length cfg.environmentFiles) > 0
          || cfg.credentialFiles != { };
        message = "OPENCROW_NOSTR_PRIVATE_KEY_FILE, environmentFiles, or credentialFiles is required when OPENCROW_BACKEND is nostr.";
      }
    ];

    # Default SEDIMENT_DB when memory extension is enabled.
    services.opencrow.environment.SEDIMENT_DB = lib.mkIf (cfg.extensions.memory or false == true) (
      lib.mkDefault "/var/lib/opencrow/sediment"
    );

    # Host-side wrapper for interactive pi usage inside the container.
    environment.systemPackages = [ opencrowPi ];

    # Host-side directory needed for the bind mount into the container.
    systemd.tmpfiles.rules = [
      "d /var/lib/opencrow 0750 - - -"
    ];

    # Work around stale machined registration after unclean shutdown.
    systemd.services."container@opencrow".preStart = lib.mkBefore ''
      ${pkgs.systemd}/bin/busctl call org.freedesktop.machine1 \
        /org/freedesktop/machine1 \
        org.freedesktop.machine1.Manager \
        UnregisterMachine s opencrow 2>/dev/null || true
    '';

    containers.opencrow = {
      autoStart = true;
      privateNetwork = false;

      bindMounts = {
        "/var/lib/opencrow" = {
          hostPath = "/var/lib/opencrow";
          isReadOnly = false;
        };
      }
      // lib.listToAttrs (
        lib.imap0 (i: path: {
          name = "/run/secrets/opencrow-envfile-${toString i}";
          value = {
            hostPath = toString path;
            isReadOnly = true;
          };
        }) cfg.environmentFiles
      )
      // cfg.extraBindMounts;

      extraFlags = lib.mapAttrsToList (
        name: path: "--load-credential=${name}:${toString path}"
      ) cfg.credentialFiles;

      config =
        { pkgs, ... }:
        {
          system.stateVersion = "25.05";

          # Work around broken pam_lastlog2.so missing libpam linkage in systemd 259
          # https://github.com/NixOS/nixpkgs/issues/493934
          # TODO: remove once https://github.com/NixOS/nixpkgs/pull/495347 lands in nixos-unstable
          security.pam.services.login.updateWtmp = lib.mkForce false;

          users.users.opencrow = {
            isSystemUser = true;
            group = "opencrow";
            home = "/var/lib/opencrow";
          };
          users.groups.opencrow = { };

          # Place the generated settings.json into PI_CODING_AGENT_DIR
          # so pi discovers declared extensions and packages.
          systemd.tmpfiles.rules = [
            "d ${cfg.environment.PI_CODING_AGENT_DIR} 0750 opencrow opencrow -"
            "L+ ${cfg.environment.PI_CODING_AGENT_DIR}/settings.json - - - - ${piSettingsJson}"
          ];

          systemd.services.opencrow = {
            description = "OpenCrow Messaging Bot";
            wantedBy = [ "multi-user.target" ];
            after = [ "network-online.target" ];
            wants = [ "network-online.target" ];

            path = [
              opencrowPkg
              cfg.piPackage
              pkgs.bash
              pkgs.coreutils
            ]
            ++ lib.optional (cfg.environment.OPENCROW_BACKEND == "signal") cfg.signalCliPackage
            ++ cfg.extraPackages;

            environment = {
              HOME = "/var/lib/opencrow";
            }
            // lib.filterAttrs (_: v: v != "") cfg.environment;

            serviceConfig = {
              EnvironmentFile = lib.imap0 (
                i: _: "/run/secrets/opencrow-envfile-${toString i}"
              ) cfg.environmentFiles;
              ImportCredential = lib.attrNames cfg.credentialFiles;
              ExecStart = lib.getExe opencrowPkg;
              Restart = "on-failure";
              RestartSec = 10;
              User = "opencrow";
              Group = "opencrow";
              WorkingDirectory = "/var/lib/opencrow";
              StateDirectory = "opencrow";
              StateDirectoryMode = "0750";
            };
          };

          environment.systemPackages = [
            opencrowPkg
            cfg.piPackage
          ]
          ++ lib.optional (cfg.environment.OPENCROW_BACKEND == "signal") cfg.signalCliPackage
          ++ cfg.extraPackages;
        };
    };
  };
}
