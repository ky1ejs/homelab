# Periphery on the Mac Studio, at the release Cut's CI pins (./release.nix),
# so a session can run Cut's ios/scripts/periphery-scan.sh. That script
# stops when `periphery` is not on PATH, and installs nothing itself.
#
# What this sets, on every apply: `periphery` on PATH, for every user.
#
# The CI VMs do not see the host's PATH. mac/ci/image-setup installs the same
# release in the image.
{ pkgs, ... }:

{
  environment.systemPackages = [
    (pkgs.callPackage ./package.nix { release = import ./release.nix; })
  ];
}
