# The connector's server, ./main.go. Its own file so CI can build it on
# Linux (mac/flake.nix's packages.x86_64-linux), which checks vendorHash and
# the build without a Mac; ./default.nix builds the same file for the mac
# studio.
#
# vendorHash covers go.sum's modules. After a change to go.mod, set it to
# lib.fakeHash, build, and copy the hash the error gives.
{ lib, buildGo127Module }:

buildGo127Module {
  pname = "mac-diag-mcp";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./go.sum
      (lib.fileset.fileFilter (f: f.hasExt "go") ./.)
    ];
  };

  vendorHash = "sha256-V03nmS8+yEo8z88RNEKrXuZd2WH5Qs2tWdwECIQAgMk=";

  env.CGO_ENABLED = "0";
  ldflags = [ "-s" "-w" ];
  # CI runs the tests (check-mac.yml), with the race detector. In the Nix
  # sandbox on macOS their local TLS servers may not bind.
  doCheck = false;

  postInstall = ''
    mv $out/bin/diag-mcp $out/bin/mac-diag-mcp
  '';

  meta.mainProgram = "mac-diag-mcp";
}
