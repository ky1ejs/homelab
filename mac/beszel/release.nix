# The Beszel agent release the mac studio runs. It must be the hub's own
# version: monitoring/docker-compose.yml pins the hub and the NAS's agent at
# the same tag, and check-mac.yml fails when this version differs from it.
# Upgrade all three in one change.
#
# The sha256 is the hex digest of beszel-agent_darwin_arm64.tar.gz, as the
# release's beszel_<version>_checksums.txt lists it; fetchurl takes it as is.
{
  version = "0.21.0";
  sha256 = "afafeaa9a34f9bfd9326c8c7077a83b3d6fb0f6e053dde17ad03fe3242bde274";
}
