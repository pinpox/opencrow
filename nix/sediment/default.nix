{
  lib,
  rustPlatform,
  fetchFromGitHub,
  aws-lc,
  oniguruma,
  openssl,
  pkg-config,
  protobuf,
  sqlite,
  zstd,
}:

rustPlatform.buildRustPackage rec {
  pname = "sediment";
  version = "0.5.1";

  src = fetchFromGitHub {
    owner = "rendro";
    repo = "sediment";
    tag = "v${version}";
    hash = "sha256-hINSwWJE9/Nq5QT2Y7vgFlrwz4fGVYhT4f98Eb7CS2c=";
  };

  cargoHash = "sha256-NfXChnMYyNyyT3ocdT65Ic6Iu3Zp0LtuTR/Je8FzqZc=";

  nativeBuildInputs = [
    pkg-config
    protobuf
  ];

  # nixpkgs' cargo setup hook makes the -sys crates link system libraries
  # instead of building their bundled copies.
  buildInputs = [
    aws-lc
    oniguruma
    openssl
    sqlite
    zstd
  ];

  env = {
    PROTOC = "${protobuf}/bin/protoc";
    PROTOC_INCLUDE = "${protobuf}/include";
  };

  # Tests require network access for model downloads
  doCheck = false;

  meta = {
    description = "Semantic memory for AI agents - local-first, MCP-native";
    homepage = "https://github.com/rendro/sediment";
    license = lib.licenses.mit;
    mainProgram = "sediment";
  };
}
