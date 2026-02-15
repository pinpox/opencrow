{
  buildGoModule,
}:
buildGoModule {
  pname = "opencrow";
  version = "0.3.0";
  src = ./..;
  vendorHash = "sha256-sIircnDhd0cmsq+rmR0FxZDGms4NKuHqQLzp4CMxBeU=";
  tags = [ "goolm" ];

  postInstall = ''
    mkdir -p $out/share/opencrow
    cp -r skills $out/share/opencrow/skills
    cp SOUL.md $out/share/opencrow/SOUL.md
  '';

  meta = {
    description = "Matrix bot bridging messages to an AI coding agent via pi RPC";
    homepage = "https://github.com/pinpox/opencrow";
    mainProgram = "opencrow";
  };
}
