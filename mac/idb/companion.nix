# idb_companion: the macOS server that drives a simulator for the idb client.
# Meta ships it prebuilt and signed. nixpkgs has only 1.1.8, from 2022, which
# predates the Xcode 26 this release needs.
{ lib, stdenvNoCC, fetchurl, release }:

stdenvNoCC.mkDerivation {
  pname = "idb-companion";
  inherit (release) version;

  src = fetchurl {
    url = "https://github.com/facebook/idb/releases/download/v${release.version}/idb-companion.macos-arm64.tar.gz";
    hash = release.companionHash;
  };

  # The archive's top level is the tree itself: idb_companion, idb-repl,
  # Resources/ and the SwiftPM resource bundles. It is unpacked straight into
  # $out, since unpacking into the build folder would mix in the builder's own
  # files.
  dontUnpack = true;

  # idb_companion finds Resources/ beside its own binary and crashes without
  # it, so the tree stays whole in libexec and only the executables are linked,
  # as Meta's formula does. No fixup: stripping or patching a signed binary
  # breaks its signature.
  dontFixup = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out/libexec/idb-companion $out/bin
    tar -xzf $src -C $out/libexec/idb-companion
    for exe in idb_companion idb-repl; do
      test -x "$out/libexec/idb-companion/$exe"
      ln -s "$out/libexec/idb-companion/$exe" "$out/bin/$exe"
    done
    test -d $out/libexec/idb-companion/Resources
    runHook postInstall
  '';

  meta = {
    description = "Companion server for automating iOS simulators and devices";
    homepage = "https://fbidb.io";
    license = lib.licenses.mit;
    platforms = [ "aarch64-darwin" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    mainProgram = "idb_companion";
  };
}
