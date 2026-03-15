{
  lib,
  rustPlatform,
  fetchFromGitHub,
  protobuf,
}:

rustPlatform.buildRustPackage {
  pname = "sediment";
  version = "0.5.0-unstable-2026-02-14";

  # TODO: switch back to rendro/sediment once
  # https://github.com/rendro/sediment/pull/60 is merged.
  src = fetchFromGitHub {
    owner = "Mic92";
    repo = "sediment";
    rev = "392158db48334bee646a2fb2d10d63465f9d02c2";
    hash = "sha256-29IuEF7RqjTlJwkf7hMB28fllUquadgQNwKq81icVlc=";
  };

  cargoHash = "sha256-6POb6FkDItoICpn/Q55XZr2LPOj3KBL2DrQMJCLcpJQ=";

  nativeBuildInputs = [ protobuf ];

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
