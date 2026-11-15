# The Periphery release Cut's dead code check uses: agoodkind/periphery, a
# community fork of the archived peripheryapp/periphery. Upstream's last
# release, 3.6.0, reports @State read only through $binding as unused under
# Xcode 27.
#
# Both values are copied from PERIPHERY_VERSION and PERIPHERY_SHA256 in Cut's
# .github/workflows/ios-build.yaml, so they move together. The sha256 is the
# hex digest of the macOS arm64 zip, as `shasum -a 256` prints it; fetchurl
# takes it as is.
#
# Two places use it: ./package.nix, on PATH for the sessions, and
# mac/ci/image-setup, which installs it in the CI image at ~/periphery, where
# Cut's workflow looks before it downloads.
{
  version = "3.9.12-agoodkind.1";
  sha256 = "5ac86b08e6535ab8b6ef7f48b161949f9ce6cd3b3101f71fa233ec0d234e5445";
}
