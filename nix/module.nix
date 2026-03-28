{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.opencrow;

  jsonFormat = pkgs.formats.json { };

  # Derive container name and state directory from instance name.
  # The "default" instance (top-level enable) keeps the original
  # paths for backward compatibility.
  containerNameOf = name: if name == "default" then "opencrow" else "opencrow-${name}";
  stateDirOf = name: "/var/lib/${containerNameOf name}";

  # Shared option definitions used by both the top-level (default instance)
  # and each named instance submodule.
  mkInstanceOptions =
    { name, config }:
    let
      opencrowPkg = config.package;
      stateDir = stateDirOf name;

      skillsDir = pkgs.linkFarm "opencrow-skills-${name}" (
        lib.mapAttrsToList (sname: path: {
          name = sname;
          inherit path;
        }) config.skills
      );
    in
    {
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
        type = lib.types.attrsOf (lib.types.either lib.types.bool lib.types.path);
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

          Bundled extensions: `memory` (cross-session recall via sediment),
          `reminders` (remind_at/list/cancel tools backed by opencrow.db).
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
        type = jsonFormat.type;
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

      piModels = lib.mkOption {
        type = jsonFormat.type;
        default = { };
        description = ''
          Contents of pi's models.json. Use this to add custom
          providers or override properties of built-in models via
          `modelOverrides` — most commonly `contextWindow` when the
          configured API tier is narrower than pi's published value
          (e.g. Anthropic's long-context requires separate usage
          credits). Without the override pi's auto-compaction never
          triggers and every turn bounces off a 429.
        '';
        example = lib.literalExpression ''
          {
            providers.anthropic.modelOverrides."claude-sonnet-4-6".contextWindow = 200000;
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
              default = lib.getExe config.signalCliPackage;
              defaultText = lib.literalExpression "lib.getExe config.services.opencrow.signalCliPackage";
              description = "Path to the signal-cli binary.";
            };

            OPENCROW_SIGNAL_CONFIG_DIR = lib.mkOption {
              type = lib.types.str;
              default = "${stateDir}/signal-cli";
              description = "Directory where signal-cli stores account data and downloaded attachments.";
            };

            OPENCROW_SIGNAL_SOCKET_PATH = lib.mkOption {
              type = lib.types.str;
              default = "${stateDir}/signal-cli/opencrow-jsonrpc.sock";
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
              default = "${stateDir}/sessions";
              description = "Directory for pi session storage.";
            };

            OPENCROW_PI_IDLE_TIMEOUT = lib.mkOption {
              type = lib.types.str;
              default = "30m";
              description = "Idle timeout for pi processes (Go duration format, e.g. 30m, 1h).";
            };

            OPENCROW_PI_WORKING_DIR = lib.mkOption {
              type = lib.types.str;
              default = stateDir;
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
              default = "${stateDir}/pi-agent";
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
              description = "Heartbeat interval (Go duration, e.g. '30m'). Empty disables heartbeat; the reminder dispatcher still runs.";
            };

            SEDIMENT_DB = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = "Path to the sediment database. Set automatically when the memory extension is enabled via the extensions option.";
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

  instanceModule =
    { name, config, ... }:
    {
      options = {
        enable = lib.mkEnableOption "OpenCrow messaging bot instance '${name}'";
      }
      // mkInstanceOptions { inherit name config; };
    };

  # Build the effective set of all enabled instances: the top-level "default"
  # instance (when services.opencrow.enable is set) merged with all named
  # instances from services.opencrow.instances.
  effectiveInstances =
    (lib.optionalAttrs cfg.enable { default = cfg; }) // lib.filterAttrs (_: i: i.enable) cfg.instances;

  mkInstanceConfig =
    name: icfg:
    let
      opencrowPkg = icfg.package;

      containerName = containerNameOf name;
      stateDir = stateDirOf name;

      # Resolve extension values: `true` means use the corresponding
      # extension package from the opencrow flake, a path is used as-is.
      resolvedExtensions = lib.mapAttrs (
        ename: value:
        if value == true then self.packages.${pkgs.hostPlatform.system}."extension-${ename}" else value
      ) (lib.filterAttrs (_: v: v != false) icfg.extensions);

      # Generate a settings.json for pi that lists declared extensions.
      # Installed into PI_CODING_AGENT_DIR at service startup so pi
      # auto-discovers them.
      piSettingsJson = jsonFormat.generate "pi-settings-${name}.json" (
        icfg.piSettings // { extensions = lib.attrValues resolvedExtensions; }
      );

      piModelsJson = jsonFormat.generate "pi-models-${name}.json" icfg.piModels;

      # Host-side wrapper to interact with pi inside the container as the opencrow user.
      opencrowPi = pkgs.writeShellScriptBin "${containerName}-pi" ''
        exec machinectl shell opencrow@${containerName} \
          /run/current-system/sw/bin/env \
            HOME=${stateDir} \
            PI_CODING_AGENT_DIR=${icfg.environment.PI_CODING_AGENT_DIR} \
          ${lib.getExe icfg.piPackage} "$@"
      '';

      # Host-side wrapper to run signal-cli inside the container as the opencrow user.
      # Needed for account setup: register, verify, link, etc.
      opencrowSignalCli = pkgs.writeShellScriptBin "${containerName}-signal-cli" ''
        exec machinectl shell opencrow@${containerName} \
          /run/current-system/sw/bin/env \
            HOME=${stateDir} \
          ${lib.getExe icfg.signalCliPackage} \
            --config ${icfg.environment.OPENCROW_SIGNAL_CONFIG_DIR} \
            "$@"
      '';
    in
    {
      assertions = [
        {
          assertion =
            icfg.environment.OPENCROW_BACKEND != "matrix" || icfg.environment.OPENCROW_MATRIX_HOMESERVER != "";
          message = "services.opencrow (${name}): OPENCROW_MATRIX_HOMESERVER is required when OPENCROW_BACKEND is matrix.";
        }
        {
          assertion =
            icfg.environment.OPENCROW_BACKEND != "signal" || icfg.environment.OPENCROW_SIGNAL_ACCOUNT != "";
          message = "services.opencrow (${name}): OPENCROW_SIGNAL_ACCOUNT is required when OPENCROW_BACKEND is signal.";
        }
        {
          assertion =
            icfg.environment.OPENCROW_BACKEND != "nostr" || icfg.environment.OPENCROW_NOSTR_RELAYS != "";
          message = "services.opencrow (${name}): OPENCROW_NOSTR_RELAYS is required when OPENCROW_BACKEND is nostr.";
        }
        {
          assertion =
            icfg.environment.OPENCROW_BACKEND != "nostr"
            || icfg.environment.OPENCROW_NOSTR_PRIVATE_KEY_FILE != ""
            # Key may also be provided via environmentFiles or credentialFiles
            || (builtins.length icfg.environmentFiles) > 0
            || icfg.credentialFiles != { };
          message = "services.opencrow (${name}): OPENCROW_NOSTR_PRIVATE_KEY_FILE, environmentFiles, or credentialFiles is required when OPENCROW_BACKEND is nostr.";
        }
      ];

      systemPackages = [
        opencrowPi
      ]
      ++ lib.optional (icfg.environment.OPENCROW_BACKEND == "signal") opencrowSignalCli;

      # The opencrow user only exists inside the container, not on the host,
      # so we cannot reference it in host-side tmpfiles rules (systemd-tmpfiles
      # would fail to resolve the name and skip the rule, leaving the bind
      # mount source missing). Create the directory as root here; the
      # container's own tmpfiles rules fix up ownership from the inside
      # where the opencrow UID is known.
      tmpfilesRules = [
        "d ${stateDir} 0750 - - -"
      ];

      containerPreStart = ''
        ${pkgs.systemd}/bin/busctl call org.freedesktop.machine1 \
          /org/freedesktop/machine1 \
          org.freedesktop.machine1.Manager \
          UnregisterMachine s ${containerName} 2>/dev/null || true
      '';

      container = {
        autoStart = true;
        privateNetwork = false;

        bindMounts = {
          "${stateDir}" = {
            hostPath = stateDir;
            isReadOnly = false;
          };
        }
        // lib.listToAttrs (
          lib.imap0 (i: path: {
            name = "/run/secrets/${containerName}-envfile-${toString i}";
            value = {
              hostPath = toString path;
              isReadOnly = true;
            };
          }) icfg.environmentFiles
        )
        // icfg.extraBindMounts;

        extraFlags = lib.mapAttrsToList (
          cname: path: "--load-credential=${cname}:${toString path}"
        ) icfg.credentialFiles;

        config =
          { pkgs, ... }:
          {
            system.stateVersion = "25.05";

            users.users.opencrow = {
              isSystemUser = true;
              group = "opencrow";
              home = stateDir;
            };
            users.groups.opencrow = { };

            # Place the generated settings.json into PI_CODING_AGENT_DIR
            # so pi discovers declared extensions and packages.
            systemd.tmpfiles.rules = [
              # Fix up ownership of the bind-mounted state dir. The host side
              # created it as root because the opencrow user does not exist
              # there; inside the container we know the UID and can chown it.
              "d ${stateDir} 0750 opencrow opencrow -"
              "d ${icfg.environment.PI_CODING_AGENT_DIR} 0750 opencrow opencrow -"
              "L+ ${icfg.environment.PI_CODING_AGENT_DIR}/settings.json - - - - ${piSettingsJson}"
            ]
            ++ lib.optional (
              icfg.piModels != { }
            ) "L+ ${icfg.environment.PI_CODING_AGENT_DIR}/models.json - - - - ${piModelsJson}";

            systemd.services.opencrow = {
              description = "OpenCrow Messaging Bot (${name})";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];

              path = [
                opencrowPkg
                icfg.piPackage
                pkgs.bash
                pkgs.coreutils
              ]
              ++ lib.optional (icfg.environment.OPENCROW_BACKEND == "signal") icfg.signalCliPackage
              ++ icfg.extraPackages;

              environment = {
                HOME = stateDir;
              }
              // lib.filterAttrs (_: v: v != "") icfg.environment
              // lib.optionalAttrs (icfg.extensions.memory or false == true) {
                SEDIMENT_DB = "${stateDir}/sediment";
              };

              serviceConfig = {
                EnvironmentFile = lib.imap0 (
                  i: _: "/run/secrets/${containerName}-envfile-${toString i}"
                ) icfg.environmentFiles;
                ImportCredential = lib.attrNames icfg.credentialFiles;
                ExecStart = lib.getExe opencrowPkg;
                Restart = "on-failure";
                RestartSec = 10;
                User = "opencrow";
                Group = "opencrow";
                WorkingDirectory = stateDir;
                StateDirectory = containerName;
                StateDirectoryMode = "0750";
              };
            };

            environment.systemPackages = [
              opencrowPkg
              icfg.piPackage
            ]
            ++ lib.optional (icfg.environment.OPENCROW_BACKEND == "signal") icfg.signalCliPackage
            ++ icfg.extraPackages;
          };
      };

    };

  instanceConfigs = lib.mapAttrs mkInstanceConfig effectiveInstances;
in
{
  options.services.opencrow = lib.mkOption {
    type = lib.types.submodule (
      { config, ... }:
      {
        options = {
          enable = lib.mkEnableOption "OpenCrow default instance";

          instances = lib.mkOption {
            type = lib.types.attrsOf (lib.types.submodule instanceModule);
            default = { };
            description = "Named OpenCrow messaging bot instances. Each instance runs in its own container.";
            example = lib.literalExpression ''
              {
                mybot = {
                  enable = true;
                  piPackage = llm-agents.packages.''${system}.pi;
                  environment = {
                    OPENCROW_BACKEND = "nostr";
                    OPENCROW_NOSTR_RELAYS = "wss://relay.damus.io";
                  };
                };
              }
            '';
          };
        }
        // mkInstanceOptions {
          name = "default";
          inherit config;
        };
      }
    );
    default = { };
    description = ''
      OpenCrow messaging bot configuration. Use `enable` and the top-level
      options for a single default instance, or `instances.<name>` for
      multiple named instances with independent containers and data.
    '';
  };

  config = lib.mkIf (instanceConfigs != { }) (
    lib.mkMerge [
      # Aggregate host-level config from all instances.
      {
        assertions = [
          {
            assertion = !(cfg.instances ? "default");
            message = "services.opencrow: the instance name 'default' is reserved for the top-level configuration. Use a different name or configure via services.opencrow.enable with top-level options.";
          }
        ]
        ++ lib.concatLists (lib.mapAttrsToList (_: ic: ic.assertions) instanceConfigs);

        environment.systemPackages = lib.concatLists (
          lib.mapAttrsToList (_: ic: ic.systemPackages) instanceConfigs
        );

        systemd.tmpfiles.rules = lib.concatLists (
          lib.mapAttrsToList (_: ic: ic.tmpfilesRules) instanceConfigs
        );

        # Work around stale machined registration after unclean shutdown.
        systemd.services = lib.mapAttrs' (
          name: ic:
          lib.nameValuePair "container@${containerNameOf name}" {
            preStart = lib.mkBefore ic.containerPreStart;
          }
        ) instanceConfigs;

        containers = lib.mapAttrs' (
          name: ic: lib.nameValuePair (containerNameOf name) ic.container
        ) instanceConfigs;
      }
    ]
  );
}
