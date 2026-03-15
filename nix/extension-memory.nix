{
  runCommand,
  callPackage,
}:
let
  sediment = callPackage ./sediment { };
in
runCommand "opencrow-extension-memory"
  {
    src = ../extensions/memory;
    inherit sediment;
  }
  ''
    mkdir -p $out
    cp -r $src/* $out/
    substituteInPlace $out/index.ts \
      --replace-fail '@@SEDIMENT_BIN@@' "$sediment/bin/sediment"
  ''
