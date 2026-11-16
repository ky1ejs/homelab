# The mac studio's Beszel agent: CPU, memory, swap, load, disk and
# temperatures for the monitoring hub on the NAS, which alerts on them and
# puts them in a weekly email (monitoring/README.md, "The mac studio").
#
# The hub pulls; the mac studio starts nothing. The hub connects to
# 192.168.40.2:45876 over SSH, and the agent answers only the hub's key,
# mac/keys/beszel-hub.pub. Three things keep anything else out:
# - it listens on the LAN address alone, so the CI VMs on Tart's bridge and
#   the tailnet address get no listener at all;
# - the gateway lets only the NAS reach that port (docs/mac-studio.md,
#   "Turning it on");
# - it runs as `nobody`, so what it can read is what any user can.
#
# What it cannot see, and what covers it instead: memory pressure, the Remote
# Control servers and the CI runners are the heartbeats' (mac/claude/heartbeat,
# mac/ci/heartbeat); powermetrics' GPU and thermal readings need root, and
# mac-usage records them; Colima's containers are worker's alone.
{ config, lib, pkgs, ... }:

let
  release = import ./release.nix;
  agent = pkgs.callPackage ./package.nix { inherit release; };

  # docs/mac-studio.md, "The network": the gateway gives the mac studio this
  # address, and the hub's MON_MAC_STUDIO_HOST names it. If it ever changes,
  # the agent cannot listen and the hub's Status alert says so.
  lanAddress = "192.168.40.2";
  port = 45876;

  # The hub's public key, one ssh-ed25519 line. Comment lines are skipped,
  # by the agent too.
  keyFile = ../keys/beszel-hub.pub;
  keyLines = builtins.filter
    (l: l != "" && !(lib.hasPrefix "#" l))
    (map lib.trim (lib.splitString "\n" (builtins.readFile keyFile)));

  dataDir = "/var/db/beszel-agent";
  logFile = "/var/log/beszel-agent.log";
in
{
  assertions = [{
    assertion = lib.all (l: builtins.match "ssh-ed25519 [A-Za-z0-9+/]+=*( .*)?" l != null) keyLines;
    message = "mac/keys/beszel-hub.pub must hold only the hub's ssh-ed25519 key (MON_HUB_KEY in monitoring/.env) and comment lines.";
  }];

  warnings = lib.optional (keyLines == [ ]) ''
    mac/keys/beszel-hub.pub has no key, so the Beszel agent is not installed
    and the hub cannot monitor the mac studio. Copy MON_HUB_KEY from the NAS's
    monitoring/.env into it (docs/mac-studio.md, "Monitoring").
  '';

  launchd.daemons = lib.mkIf (keyLines != [ ]) {
    beszel-agent.serviceConfig = {
      ProgramArguments = [ "${agent}/bin/beszel-agent" ];
      UserName = "nobody";
      GroupName = "nobody";
      RunAtLoad = true;
      # Restarted when it exits, including when the LAN address is not up yet
      # at boot and the listen fails.
      KeepAlive = true;
      ThrottleInterval = 30;
      Nice = 10;
      StandardOutPath = logFile;
      StandardErrorPath = logFile;
      EnvironmentVariables = {
        LISTEN = "${lanAddress}:${toString port}";
        KEY_FILE = "${keyFile}";
        # Where the agent keeps its fingerprint, which the hub uses to tell a
        # system apart from a new one at the same address.
        DATA_DIR = dataDir;
        # APFS mounts a sealed, read-only system volume at /, so its usage
        # never moves. The data volume is where everything that fills the
        # disk lives: CI images, Xcode, worker's checkouts.
        FILESYSTEM = "/System/Volumes/Data";
        # Docker is worker's Colima, which nobody cannot reach. Empty turns
        # the agent's Docker client off instead of retrying it.
        DOCKER_HOST = "";
        # The GPU collector on macOS runs powermetrics, which needs root.
        SKIP_GPU = "true";
      };
    };
  };

  # Before launchd loads the daemon, which comes before postActivation: a
  # data directory and a log file that nobody can write. launchd makes
  # neither for a job's own user.
  system.activationScripts.preActivation.text = lib.mkIf (keyLines != [ ]) ''
    # Monitoring: the Beszel agent's directory and log (mac/beszel).
    install -d -m 700 -o nobody -g nobody ${dataDir}
    touch ${logFile}
    chown nobody:admin ${logFile}
    chmod 640 ${logFile}
  '';
}
