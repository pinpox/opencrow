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
in
{
  options.services.opencrow = {
    enable = lib.mkEnableOption "OpenCrow Matrix bot";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.hostPlatform.system}.opencrow;
      defaultText = lib.literalExpression "opencrow.packages.\${system}.opencrow";
      description = "The opencrow package to use.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        Path to an environment file containing secrets (on the host).
        Bind-mounted read-only into the container.
        Must define at minimum:
        - OPENCROW_MATRIX_ACCESS_TOKEN
        - ANTHROPIC_API_KEY (or the appropriate key for your provider)
      '';
    };

    extraEnvironmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.path;
      default = [ ];
      description = "Additional environment files bind-mounted into the container.";
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
          OPENCROW_MATRIX_HOMESERVER = lib.mkOption {
            type = lib.types.str;
            description = "Matrix homeserver URL.";
            example = "https://matrix.example.com";
          };

          OPENCROW_MATRIX_DEVICE_ID = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Matrix device ID.";
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
            description = "Directory for pi session storage (per-room subdirectories).";
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
            default = "${opencrowPkg}/share/opencrow/skills/web";
            description = "Comma-separated list of skill paths to pass to pi via --skill.";
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

          OPENCROW_HEARTBEAT_INTERVAL = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Heartbeat interval (Go duration, e.g. '30m'). Empty disables heartbeat.";
          };

          OPENCROW_HEARTBEAT_TRIGGER_DIR = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/opencrow/triggers";
            description = "Directory for trigger files that wake the heartbeat.";
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

    # State directory on host (bind-mounted into container)
    systemd.tmpfiles.rules = [
      "d /var/lib/opencrow 0750 root root -"
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
        "/run/secrets/opencrow-envfile" = {
          hostPath = toString cfg.environmentFile;
          isReadOnly = true;
        };
      }
      // lib.listToAttrs (
        map (path: {
          name = "/run/secrets/opencrow-extra-${baseNameOf (toString path)}";
          value = {
            hostPath = toString path;
            isReadOnly = true;
          };
        }) cfg.extraEnvironmentFiles
      )
      // cfg.extraBindMounts;

      config =
        { ... }:
        {
          system.stateVersion = "25.05";

          systemd.services.opencrow = {
            description = "OpenCrow Matrix Bot";
            wantedBy = [ "multi-user.target" ];
            after = [ "network-online.target" ];
            wants = [ "network-online.target" ];

            path = [ opencrowPkg ] ++ cfg.extraPackages;

            environment = cfg.environment;

            serviceConfig = {
              EnvironmentFile = [
                "/run/secrets/opencrow-envfile"
              ]
              ++ map (
                path: "/run/secrets/opencrow-extra-${baseNameOf (toString path)}"
              ) cfg.extraEnvironmentFiles;
              ExecStart = lib.getExe opencrowPkg;
              Restart = "on-failure";
              RestartSec = 10;
              WorkingDirectory = "/var/lib/opencrow";
            };
          };

          environment.systemPackages = [ opencrowPkg ] ++ cfg.extraPackages;
        };
    };
  };
}
