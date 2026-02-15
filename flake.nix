{
  description = "OpenCrow - Matrix bot bridging messages to an AI coding agent via pi RPC";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      forAllSystems = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
      ];
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          opencrow = pkgs.buildGoModule {
            pname = "opencrow";
            version = "0.3.0";
            src = ./.;
            vendorHash = "sha256-sIircnDhd0cmsq+rmR0FxZDGms4NKuHqQLzp4CMxBeU=";
            tags = [ "goolm" ];

            postInstall = ''
              mkdir -p $out/share/opencrow
              cp -r skills $out/share/opencrow/skills
            '';

            meta = {
              description = "Matrix bot bridging messages to an AI coding agent via pi RPC";
              homepage = "https://github.com/pinpox/opencrow";
              mainProgram = "opencrow";
            };
          };

          default = self.packages.${system}.opencrow;
        }
      );

      nixosModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.services.opencrow;
        in
        {
          options.services.opencrow = {
            enable = lib.mkEnableOption "OpenCrow Matrix bot";

            package = lib.mkPackageOption pkgs "opencrow" {
              default = self.packages.${pkgs.system}.opencrow;
            };

            environmentFile = lib.mkOption {
              type = lib.types.path;
              description = ''
                Path to an environment file containing secrets.
                Must define at minimum:
                - OPENCROW_MATRIX_ACCESS_TOKEN
                - ANTHROPIC_API_KEY (or the appropriate key for your provider)
              '';
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

                  OPENCROW_MATRIX_USER_ID = lib.mkOption {
                    type = lib.types.str;
                    description = "Matrix user ID for the bot.";
                    example = "@opencrow:example.com";
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
                    default = "claude-sonnet-4-5-20250929";
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
                    default = "${cfg.package}/share/opencrow/skills/web";
                    description = "Comma-separated list of skill paths to pass to pi via --skill.";
                  };

                  PI_CODING_AGENT_DIR = lib.mkOption {
                    type = lib.types.str;
                    default = "/var/lib/opencrow/pi-agent";
                    description = "Directory where pi stores its agent configuration and data.";
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
            systemd.services.opencrow = {
              description = "OpenCrow Matrix Bot";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];

              environment = cfg.environment;

              serviceConfig = {
                ExecStart = lib.getExe cfg.package;
                EnvironmentFile = cfg.environmentFile;
                StateDirectory = "opencrow";
                WorkingDirectory = "/var/lib/opencrow";
                Restart = "on-failure";
                RestartSec = 10;

                # Hardening
                DynamicUser = true;
                NoNewPrivileges = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                PrivateTmp = true;
              };
            };
          };
        };
    };
}
