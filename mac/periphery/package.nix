# Periphery, the Swift dead code scanner, from the fork's prebuilt, signed
# release. nixpkgs has no package for the fork.
{ lib, stdenvNoCC, fetchurl, unzip, release }:

stdenvNoCC.mkDerivation {
  pname = "periphery";
  inherit (release) version;

  src = fetchurl {
    url = "https://github.com/agoodkind/periphery/releases/download/${release.version}/periphery-${release.version}-macos-arm64.zip";
    inherit (release) sha256;
  };

  nativeBuildInputs = [ unzip ];

  # The zip holds one folder, periphery-<version>, with the binary and
  # libIndexStore.dylib.
  sourceRoot = "periphery-${release.version}";

  # The binary finds libIndexStore.dylib through an @loader_path rpath, so
  # both stay together in libexec and only the binary is linked into bin, as
  # mac/idb does. dyld expands @loader_path from the binary's real path, so
  # the link finds the dylib; the install check below runs it through the
  # link. No fixup: stripping or patching a signed binary breaks its
  # signature.
  dontFixup = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out/libexec/periphery
    install -m 755 periphery $out/libexec/periphery/periphery
    install -m 644 libIndexStore.dylib $out/libexec/periphery/libIndexStore.dylib
    mkdir -p $out/bin
    ln -s $out/libexec/periphery/periphery $out/bin/periphery
    runHook postInstall
  '';

  # The check Cut's workflow makes after installing it: the binary runs and
  # reports this release.
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    reported="$(HOME="$TMPDIR" $out/bin/periphery version)"
    if [ "$reported" != ${lib.escapeShellArg release.version} ]; then
      echo "periphery reports '$reported', expected ${release.version}" >&2
      exit 1
    fi
    runHook postInstallCheck
  '';

  meta = {
    description = "Finds unused code in Swift projects";
    homepage = "https://github.com/agoodkind/periphery";
    license = lib.licenses.mit;
    platforms = [ "aarch64-darwin" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    mainProgram = "periphery";
  };
}
