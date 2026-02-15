{
  description = "OpenCrow - Personal AI assistent connecting via Matrix";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      forAllSystems = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          opencrow = pkgs.callPackage ./nix/package.nix { };
          default = self.packages.${system}.opencrow;
        }
      );

      nixosModules.default =
        nixpkgs.lib.modules.importApply ./nix/module.nix { inherit self; };
    };
}
