# Slim iOS simulators on the mac studio, with simslim
# (https://github.com/MobAI-App/simslim).
#
# A stock iOS 27 simulator runs about 290 processes. On the mac studio
# (macOS 27.2.1, Xcode 27.1) one took 12.97 GB of memory by phys_footprint;
# slimmed, 75 processes and 2.34 GB (measured 2026-11-20). simslim writes
# launchd disable overrides into the simulator's own store, so it changes
# nothing on the host. Upstream issue #73: on macOS 27.2 hosts a slim
# simulator still takes about 2.5 GB, against 620 MB on macOS 27.0, with no
# fix yet.
#
# What this sets, on every apply once worker exists:
# - `simslim` on PATH;
# - a launchd agent in worker's session running `simslim watch`, which slims
#   every simulator worker has: a shut-down one before it boots, and each
#   one again as it boots, Xcode's parallel-testing clones included;
# - `sim-full` (./sim-full), which turns the services back on in one
#   simulator until it shuts down.
#
# A slim simulator has no push registration, Sign in with Apple, iCloud,
# universal links, StoreKit or Spotlight. Contacts and Photos stay on
# (`except` below): Cut's friend finder reads Contacts and its image picker
# uses Photos. mac/claude/worker-CLAUDE.md tells sessions when to run
# sim-full.
#
# Only worker runs the agent. kylejs's simulators, in Xcode on the screen,
# stay stock.
{ lib, pkgs, ... }:

let
  user = "worker";
  home = "/Users/${user}";

  release = import ./release.nix;
  simslim = pkgs.callPackage ./package.nix { inherit release; };

  # simslim categories (`simslim profiles`) the agent leaves on.
  except = [ "pim" "photos" ];

  # sim-full reads the agent's events from here and waits more than one scan
  # for a pass to start, so a change to either is a change to ./sim-full.
  eventsFile = "${home}/Library/Logs/simslim-watch.jsonl";
  interval = "3s";

  simFull = pkgs.runCommand "sim-full" { } ''
    install -Dm755 ${./sim-full} $out/bin/sim-full
    substituteInPlace $out/bin/sim-full \
      --replace-fail '@simslim@' '${simslim}/bin/simslim' \
      --replace-fail '@jq@' '${pkgs.jq}/bin/jq'
  '';

  # xcrun, for simctl.
  path = lib.concatStringsSep ":" [
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];
in
{
  environment.systemPackages = [ simslim simFull ];

  homelab.worker.agents = [{
    Label = "dev.kylejs.simslim-watch";
    ProgramArguments = [
      "${simslim}/bin/simslim"
      "watch"
      # One JSON object per event on stdout: slimming, slimmed, failed,
      # stopped, slimmed_before_boot. A few per boot, so the file grows
      # slowly; nothing rotates it. With --json, stderr gets errors only.
      "--json"
      "--except" (lib.concatStringsSep "," except)
      "--interval" interval
    ];
    EnvironmentVariables = { PATH = path; HOME = home; };
    StandardOutPath = eventsFile;
    StandardErrorPath = "${home}/Library/Logs/simslim-watch.log";
    RunAtLoad = true;
    KeepAlive = true;
    ThrottleInterval = 30;
    # CoreSimulator runs in worker's login session.
    LimitLoadToSessionType = "Aqua";
  }];
}
