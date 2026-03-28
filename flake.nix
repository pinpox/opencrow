{
  description = "OpenCrow - Personal AI assistent connecting via Matrix";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    treefmt-nix.url = "github:numtide/treefmt-nix";
    treefmt-nix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
    }:
    let
      forAllSystems = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          opencrow = pkgs.callPackage ./nix/package.nix { };
          extension-memory = pkgs.callPackage ./nix/extension-memory.nix { };
          extension-reminders = pkgs.callPackage ./nix/extension-reminders.nix { };
          sediment = pkgs.callPackage ./nix/sediment { };
          default = self.packages.${system}.opencrow;
        }
      );

      formatter = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        (treefmt-nix.lib.evalModule pkgs ./nix/treefmt.nix).config.build.wrapper
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            inputsFrom = [ self.packages.${system}.opencrow ];
            packages = [
              pkgs.golangci-lint
              pkgs.sqlc
            ];
          };
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          formatting = (treefmt-nix.lib.evalModule pkgs ./nix/treefmt.nix).config.build.check self;

          golangci-lint = self.packages.${system}.opencrow.overrideAttrs (old: {
            nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.golangci-lint ];
            outputs = [ "out" ];
            buildPhase = ''
              HOME=$TMPDIR
              golangci-lint run --build-tags goolm
            '';
            installPhase = ''
              touch $out
            '';
          });
        }
      );

      nixosModules.default = nixpkgs.lib.modules.importApply ./nix/module.nix { inherit self; };
    };
}
