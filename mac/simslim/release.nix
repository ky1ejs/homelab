# The simslim release the mac studio runs. Its CLI is a Go binary until
# v0.15.0; an open pull request upstream ports it to Swift, so an upgrade past
# that may need ./package.nix changed, not only these two lines.
#
# The sha256 is the hex digest of simslim-v<version>-macos-arm64.tar.gz, the
# release's asset; fetchurl takes it as is.
{
  version = "0.15.0";
  sha256 = "8db2d7b9c41f7cbc925c883a2907a2941e8b8bf8d6fc8cc4ce7343a830d61a31";
}
