# The Beszel agent, from the release's prebuilt binary. nixpkgs-unstable has
# a newer agent than the hub, and 26.05 an older one; the hub's protocol
# moves with its version, so this follows the hub's pin instead (./release.nix).
{ lib, stdenvNoCC, fetchurl, release }:

stdenvNoCC.mkDerivation {
  pname = "beszel-agent";
  inherit (release) version;

  src = fetchurl {
    url = "https://github.com/henrygd/beszel/releases/download/v${release.version}/beszel-agent_darwin_arm64.tar.gz";
    inherit (release) sha256;
  };

  # The tarball holds the binary, LICENSE and readme.md at its top level.
  sourceRoot = ".";

  # Go signs its darwin binaries ad hoc when it links them; stripping one
  # breaks that signature, and macOS then refuses to run it.
  dontFixup = true;

  installPhase = ''
    runHook preInstall
    install -Dm755 beszel-agent $out/bin/beszel-agent
    runHook postInstall
  '';

  # The binary runs and reports this release.
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    reported="$($out/bin/beszel-agent --version)"
    if [ "$reported" != ${lib.escapeShellArg "beszel-agent ${release.version}"} ]; then
      echo "beszel-agent reports '$reported', expected ${release.version}" >&2
      exit 1
    fi
    runHook postInstallCheck
  '';

  meta = {
    description = "Beszel's agent: host metrics for the hub on the NAS";
    homepage = "https://github.com/henrygd/beszel";
    license = lib.licenses.mit;
    platforms = [ "aarch64-darwin" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    mainProgram = "beszel-agent";
  };
}
