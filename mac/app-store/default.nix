# `app-store` on the mac studio: worker's agent sessions build, sign and ship
# any iOS or tvOS app with one App Store Connect API key (./app-store,
# docs/mac-studio.md, "Building and shipping apps").
#
# What this sets, on every apply: the `app-store` command on PATH, for every
# user. Nothing else: no launchd agent, no file in worker's home.
#
# What it does NOT do, and why:
# - Hold the key or its IDs. This repo is public. `app-store setup`, run once
#   by hand in worker-shell, puts them in ~worker/.appstoreconnect, readable by
#   worker only; the script holds only that path.
# - Create an app's App Store Connect record. Apple's API cannot; `app-store
#   testflight` stops with exit code 3 and the steps for the website.
# - Sign Cut. Cut signs with fastlane match in its own CI, with its own key.
#   This key is a second one, so it can be revoked without touching Cut's.
{ lib, pkgs, ... }:

let
  # Fixed here, not read from the environment, so no caller can point the
  # command at another key folder. mac/cli names worker the same way.
  worker = "worker";
  keyDir = "/Users/${worker}/.appstoreconnect";
  appStore = pkgs.runCommand "app-store" { } ''
    install -Dm755 ${./app-store} $out/bin/app-store
    substituteInPlace $out/bin/app-store \
      --replace-fail '@jq@' '${pkgs.jq}/bin/jq' \
      --replace-fail '@openssl@' '${pkgs.openssl}/bin/openssl' \
      --replace-fail '@worker@' ${lib.escapeShellArg worker} \
      --replace-fail '@keyDir@' ${lib.escapeShellArg keyDir}
  '';
in
{
  environment.systemPackages = [ appStore ];
}
