# The idb release both packages come from, so the client and the companion are
# always one build. These are the artifacts Meta's own Homebrew tap installs
# (meta/fb's idb-release.json for the tag names the same files and sha256s).
# To update: take the tag and the two sha256s from that file for the new
# release, and convert each with `nix hash convert --hash-algo sha256 <hex>`.
{
  version = "1.7.8";
  companionHash = "sha256-RDaaLfC2LjoXZN5hvQkoCZnH6bJ+qPIL/x6jqdsDAqI=";
  clientHash = "sha256-7YVXpMVP/uUwBsH+Y/BwzaVnlJMSqyS+Vvr5h8Wj0Es=";
}
