# The ad hoc builds server on the mac studio: `app-store adhoc` (mac/app-store)
# writes a signed .ipa, its manifest and an install page into a random folder,
# and this serves them to the owner's devices on the tailnet, so a link opened
# in Safari on a registered iPhone installs the app. No TestFlight, no App
# Store Connect record, no tester group (docs/mac-studio.md, "Ad hoc builds").
#
# What this sets, on every apply:
# - the `builds` command on PATH (./builds);
# - a launchd agent in worker's session, dev.kylejs.builds, that waits for
#   Colima's Docker and starts ./compose.yaml: a read-only nginx and a
#   Tailscale sidecar that is its own tailnet node, tag:builds, and serves
#   it with `tailscale serve`;
# - a second agent, dev.kylejs.builds-sweep, that deletes builds older than
#   settings.nix's keepDays, hourly.
#
# What it does NOT do, and why:
# - Funnel. The builds are for the owner's own phone, which is on the
#   tailnet, so nothing is published to the internet. An ad hoc .ipa installs
#   only on registered devices, but anyone with its link could download it.
# - Log the sidecar in. That needs a Tailscale auth key, a secret, and this
#   repo is public: `builds login`, run once by hand in worker-shell, takes
#   one and never stores it.
# - Run the server as anyone but worker. Colima's Docker is worker's, so any
#   session can reach these containers. The tailnet policy makes that
#   harmless: tag:builds has no grant, so its node reaches nothing.
{ config, lib, pkgs, ... }:

let
  s = import ./settings.nix;
  home = "/Users/${s.user}";
  q = lib.escapeShellArg;

  builds = pkgs.runCommand "builds" { } ''
    install -Dm755 ${./builds} $out/bin/builds
    substituteInPlace $out/bin/builds \
      --replace-fail '@docker@' '${pkgs.docker}/bin/docker' \
      --replace-fail '@jq@' '${pkgs.jq}/bin/jq' \
      --replace-fail '@composeFile@' '${./compose.yaml}' \
      --replace-fail '@nginxConf@' '${./nginx.conf}' \
      --replace-fail '@serveJson@' '${./serve.json}' \
      --replace-fail '@worker@' ${q s.user} \
      --replace-fail '@buildsDir@' ${q s.dir} \
      --replace-fail '@confDir@' ${q s.confDir} \
      --replace-fail '@node@' ${q s.node} \
      --replace-fail '@tag@' ${q s.tag} \
      --replace-fail '@host@' ${q "${s.node}.${s.tailnet}"} \
      --replace-fail '@keepDays@' ${toString s.keepDays}
  '';

  path = lib.concatStringsSep ":" [
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];

  logFile = "${home}/Library/Logs/builds.log";

  env = {
    PATH = path;
    HOME = home;
    # Colima's socket (mac/colima).
    inherit (config.homelab.worker.environment) DOCKER_HOST;
  };
in
{
  environment.systemPackages = [ builds ];

  homelab.worker.agents = [
    {
      Label = "dev.kylejs.builds";
      ProgramArguments = [ "${builds}/bin/builds" "start" ];
      EnvironmentVariables = env;
      StandardOutPath = logFile;
      StandardErrorPath = logFile;
      # At login, beside Colima's agent: `start` waits up to 10 minutes for
      # its Docker. It starts the containers and exits; their restart policy
      # keeps them up across the VM's restarts. A failed start runs again.
      RunAtLoad = true;
      KeepAlive = { SuccessfulExit = false; };
      ThrottleInterval = 60;
      LimitLoadToSessionType = "Aqua";
    }
    {
      Label = "dev.kylejs.builds-sweep";
      ProgramArguments = [ "${builds}/bin/builds" "sweep" ];
      EnvironmentVariables = env;
      StandardOutPath = logFile;
      StandardErrorPath = logFile;
      # Hourly, so a build goes at most an hour after its keepDays.
      RunAtLoad = true;
      StartInterval = 60 * 60;
      LimitLoadToSessionType = "Aqua";
    }
  ];
}
