{
  lib,
  buildGoModule,
}:
buildGoModule (finalAttrs: {
  pname = "opencrow";
  version = "0.3.0";
  ldflags = [ "-X main.version=${finalAttrs.version}" ];
  src = lib.fileset.toSource {
    root = ./..;
    fileset = lib.fileset.unions [
      ./../go.mod
      ./../go.sum
      (lib.fileset.fileFilter (file: file.hasExt "go") ./..)
      (lib.fileset.fileFilter (file: file.hasExt "sql") ./..)
      ./../.golangci.yml
      ./../testdata
      ./../skills
      ./../SOUL.md
    ];
  };
  vendorHash = "sha256-lTNMcfe8ZTKrrhIz3CZgwiU5/OKSKIoA9eIOdhqdo7w=";
  subPackages = [ "." ];
  tags = [ "goolm" ];

  # buildGoModule only passes `tags` to `go install`, not to GOFLAGS,
  # so `nix develop` doesn't get them. Also replace -mod=vendor with
  # -mod=mod since we don't vendor locally.
  shellHook = ''
    export GOFLAGS="-mod=mod -trimpath -tags=goolm"
  '';

  postInstall = ''
    mkdir -p $out/share/opencrow
    cp -r skills $out/share/opencrow/skills
    cp SOUL.md $out/share/opencrow/SOUL.md
  '';

  meta = {
    description = "Messaging bot (Matrix/Nostr/Signal) bridging messages to an AI coding agent via pi RPC";
    homepage = "https://github.com/pinpox/opencrow";
    mainProgram = "opencrow";
  };
})
