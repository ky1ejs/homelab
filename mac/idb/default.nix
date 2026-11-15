# idb on the Mac Studio: Meta's iOS Development Bridge, which worker's Claude
# sessions use to tap, type and read the screen in an iOS simulator without
# opening Simulator on screen.
#
# A session installed it for itself on 2026-11-13, at whatever version that
# session picked: idb-companion 1.1.8 in a Nix profile and the client with
# `pip install --user` on Apple's Python 3.9 (the heartbeat's tools check,
# mac/claude/heartbeat). Here both come from one pinned release (./release.nix)
# for every session.
#
# What this sets, on every apply:
# - `idb` and `idb_companion` (with `idb-repl`) on PATH, for every user;
# - `idb` uses this release's companion unless IDB_COMPANION_PATH is set.
#
# What this does NOT do: remove the copy a session installed. docs/mac-studio.md,
# "Tools installed outside the Nix config", has the steps.
{ pkgs, nixpkgs-unstable, ... }:

let
  release = import ./release.nix;
  unstable = import nixpkgs-unstable { inherit (pkgs.stdenv.hostPlatform) system; };
  companion = pkgs.callPackage ./companion.nix { inherit release; };
  client = unstable.callPackage ./client.nix { inherit release companion; };
in
{
  environment.systemPackages = [ companion client ];
}
