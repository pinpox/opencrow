{
  buildGoModule,
}:
buildGoModule {
  pname = "opencrow";
  version = "0.3.0";
  src = ./..;
  vendorHash = "sha256-WZ70HW3c4iGg5hpfYdIenKBaPFni7IwPhizakXhzj9Q=";
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
    description = "Messaging bot (Matrix/Nostr) bridging messages to an AI coding agent via pi RPC";
    homepage = "https://github.com/pinpox/opencrow";
    mainProgram = "opencrow";
  };
}
