# idb: the Python command-line client, from the same release's wheel. The
# wheel has the generated gRPC code, so nothing is built; its generated code
# needs protobuf 7.35.1 or later at run time, newer than 26.05 has, so this is
# built with nixpkgs-unstable's Python (./default.nix).
{ lib, python3Packages, fetchurl, release, companion ? null }:

python3Packages.buildPythonApplication {
  pname = "fb-idb";
  inherit (release) version;
  format = "wheel";

  src = fetchurl {
    url = "https://github.com/facebook/idb/releases/download/v${release.version}/fb_idb-${release.version}-py3-none-any.whl";
    hash = release.clientHash;
  };

  dependencies = with python3Packages; [ aiofiles grpclib protobuf ];

  # The client looks for idb_companion through IDB_COMPANION_PATH, then on
  # PATH. Pointing it at this release's companion keeps the two one build,
  # whatever else a session puts on PATH.
  makeWrapperArgs = lib.optionals (companion != null) [
    "--set-default" "IDB_COMPANION_PATH" "${companion}/bin/idb_companion"
  ];

  pythonImportsCheck = [ "idb" "idb.cli.main" "idb.grpc.idb_pb2" ];

  meta = {
    description = "Command-line client for automating iOS simulators and devices";
    homepage = "https://fbidb.io";
    license = lib.licenses.mit;
    mainProgram = "idb";
  };
}
