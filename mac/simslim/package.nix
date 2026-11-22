# simslim, from the release's prebuilt binary. It is not in nixpkgs, and the
# release's binary is the one its README's measurements were made with.
{ lib, stdenvNoCC, fetchurl, makeWrapper, release }:

stdenvNoCC.mkDerivation {
  pname = "simslim";
  inherit (release) version;

  src = fetchurl {
    url = "https://github.com/MobAI-App/simslim/releases/download/v${release.version}/simslim-v${release.version}-macos-arm64.tar.gz";
    inherit (release) sha256;
  };

  # The tarball holds the one binary at its top level.
  sourceRoot = ".";

  nativeBuildInputs = [ makeWrapper ];

  # Go signs its darwin binaries ad hoc when it links them; stripping one
  # breaks that signature, and macOS then refuses to run it.
  dontFixup = true;

  installPhase = ''
    runHook preInstall
    install -Dm755 simslim $out/libexec/simslim
    # simslim runs ps, top, pgrep, xcrun and launchctl from PATH. A Nix
    # copy of top is not setuid root and cannot sample other processes, so
    # the system's tools go first, whatever shell or session runs simslim.
    makeWrapper $out/libexec/simslim $out/bin/simslim \
      --prefix PATH : /usr/bin:/bin:/usr/sbin:/sbin
    runHook postInstall
  '';

  # The binary runs and reports this release.
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    reported="$($out/bin/simslim --version)"
    if [ "$reported" != ${lib.escapeShellArg "simslim ${release.version}"} ]; then
      echo "simslim reports '$reported', expected ${release.version}" >&2
      exit 1
    fi
    runHook postInstallCheck
  '';

  meta = {
    description = "Turns off services a simulator does not need, to cut its memory";
    homepage = "https://github.com/MobAI-App/simslim";
    license = lib.licenses.mit;
    platforms = [ "aarch64-darwin" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    mainProgram = "simslim";
  };
}
