# The mac studio's diagnostics connector: a read-only MCP server that a
# claude.ai custom connector calls, so a question about the mac studio's
# disk, memory, CI slots or Docker needs no one at a terminal
# (docs/mac-studio.md, "Diagnostics connector").
#
# Three daemons, split by trust:
# - mac-diag-collect, root: ./mac-diag `run` every 5 minutes, and whenever
#   the connector drops a request in queue/fast. It runs every command,
#   reuses mac/usage's samples, and writes one JSON snapshot that only root
#   and _macdiag can read.
# - mac-diag-disk, root: ./mac-diag `run-disk`, mac/diagnose's disk scan
#   every 6 hours and on request in queue/disk. Its own job, so a scan of
#   several minutes holds up no host or CI figures.
# - mac-diag-mcp, _macdiag: ./main.go. It reads that snapshot and nothing
#   else, runs no command, and serves six fixed tools behind WorkOS AuthKit
#   sign-in, from its own tailnet node through Tailscale Funnel.
#
# Why Funnel, when the builds server (mac/builds) is tailnet only: claude.ai
# calls a custom connector from Anthropic's servers, which are not on the
# tailnet. The node is tag:mac-diag, which the tailnet policy grants nothing,
# so it reaches no other device; Funnel publishes this one port.
#
# Why a user of its own: the server faces the internet, so it should hold as
# little as possible. As worker, every agent session could read its tailnet
# key and stop it; as nobody, mac/beszel's agent could. _macdiag owns the
# node's state and nothing else, and reads the snapshot through its group.
#
# What is not here: the sign-in settings and the tailnet key. `sudo mac-diag
# login` writes the first beside the snapshot and joins the tailnet with the
# second, which it never stores. Until then the server exits at start.
{ config, lib, pkgs, ... }:

let
  q = lib.escapeShellArg;
  builds = import ../builds/settings.nix;

  user = "_macdiag";
  # Unused on macOS (which takes system accounts below 500) and checked
  # before the apply creates it, in preActivation below.
  id = 471;
  hostname = "mac-diag";
  resource = "https://${hostname}.${builds.tailnet}/mcp";
  # claude.ai's client id: its metadata document's URL (CIMD).
  client = "https://claude.ai/oauth/mcp-oauth-client-metadata";

  dir = "/var/db/mac-diag";
  stateDir = "/var/db/mac-diag-mcp";
  settings = "${dir}/settings";
  serverLabel = "org.nixos.mac-diag-mcp";
  collectLog = "/var/log/mac-diag-collect.log";
  diskLog = "/var/log/mac-diag-disk.log";
  serverLog = "/var/log/mac-diag-mcp.log";

  # mac/ci's slots, as mac/cli works them out.
  slots = lib.sort (a: b: lib.toInt a < lib.toInt b)
    (map (lib.removePrefix "dev.kylejs.ci-runner-")
      (lib.filter (lib.hasPrefix "dev.kylejs.ci-runner-")
        (lib.attrNames config.homelab.worker.drain.commands)));

  server = pkgs.callPackage ./package.nix { };

  tool = pkgs.runCommand "mac-diag" { } ''
    install -Dm755 ${./mac-diag} $out/bin/mac-diag
    substituteInPlace $out/bin/mac-diag \
      --replace-fail '#!/usr/bin/env python3' '#!${pkgs.python3}/bin/python3' \
      --replace-fail '"@dir@"' ${q ''"${dir}"''} \
      --replace-fail '"@macDiagnose@"' '"/run/current-system/sw/bin/mac-diagnose"' \
      --replace-fail '"@gid@"' '"${toString id}"' \
      --replace-fail '"@sw@"' '"/run/current-system/sw/bin"' \
      --replace-fail '"@server@"' '"${server}/bin/mac-diag-mcp"' \
      --replace-fail '"@user@"' ${q ''"${user}"''} \
      --replace-fail '"@label@"' ${q ''"${serverLabel}"''} \
      --replace-fail '"@hostname@"' ${q ''"${hostname}"''} \
      --replace-fail '"@stateDir@"' ${q ''"${stateDir}"''} \
      --replace-fail '"@resource@"' ${q ''"${resource}"''} \
      --replace-fail '"@settings@"' ${q ''"${settings}"''} \
      --replace-fail '"@worker@"' '"worker"' \
      --replace-fail '"@dockerHost@"' ${q ''"${config.homelab.worker.environment.DOCKER_HOST}"''}
    if grep -n '"@[a-zA-Z]*@"' $out/bin/mac-diag; then
      echo "mac/diag-mcp: a placeholder in mac-diag is left unfilled" >&2
      exit 1
    fi
  '';
in
{
  assertions = [{
    assertion = slots != [ ];
    message = "mac/diag-mcp: found no CI runner slots in homelab.worker.drain.commands";
  }];

  environment.systemPackages = [ tool ];

  users.knownUsers = [ user ];
  users.knownGroups = [ user ];
  users.users.${user} = {
    uid = id;
    gid = id;
    description = "mac studio diagnostics connector";
    home = "/var/empty";
    createHome = false;
    shell = "/usr/bin/false";
    isHidden = true;
  };
  users.groups.${user} = {
    gid = id;
    description = "mac studio diagnostics connector";
  };

  # nix-darwin skips a known user whose name has another uid, but not a uid
  # another name already has: sysadminctl would fail halfway through the
  # apply. This stops it first, before anything changes.
  system.activationScripts.preActivation.text = ''
    for kind in Users Groups; do
      if [ "$kind" = Users ]; then key=UniqueID; else key=PrimaryGroupID; fi
      owner="$(dscl . -search "/$kind" "$key" ${toString id} 2>/dev/null | awk 'NR==1 {print $1}')"
      if [ -n "$owner" ] && [ "$owner" != ${q user} ]; then
        echo "error: mac/diag-mcp: ${toString id} is $owner's $key, not ${user}'s; pick another id" >&2
        exit 1
      fi
    done
  '';

  # After the users exist and before launchd loads the daemons. The
  # snapshot's folder, with the settings in it, is root's and readable by
  # the group. queue is root's too, so _macdiag can only drop a request into
  # one job's folder in it, which root empties. The node's state is
  # _macdiag's alone. The logs launchd opens as root; the server's is
  # _macdiag's group-readable file, as mac/beszel's agent log is nobody's.
  system.activationScripts.users.text = lib.mkAfter ''
    install -d -m 750 -o root -g ${user} ${dir} ${dir}/queue
    install -d -m 700 -o ${user} -g ${user} ${dir}/queue/fast ${dir}/queue/disk
    install -d -m 700 -o ${user} -g ${user} ${stateDir}
    for f in ${collectLog} ${diskLog}; do
      touch "$f"
      chown root:admin "$f"
      chmod 640 "$f"
    done
    touch ${serverLog}
    chown ${user}:admin ${serverLog}
    chmod 640 ${serverLog}
  '';

  # The collectors' logs, capped at 1 MB with three kept: launchd opens
  # them afresh at each run, so newsyslog needs to signal nothing. The
  # server keeps its log open, so a rotation would only move where it
  # writes; it logs a rejected request at most once a minute instead.
  environment.etc."newsyslog.d/mac-diag.conf".text = ''
    # logfilename                      [owner:group] mode count size when flags
    ${collectLog}  root:admin    640  3     1024 *    NZ
    ${diskLog}     root:admin    640  3     1024 *    NZ
  '';

  launchd.daemons.mac-diag-collect.serviceConfig = {
    ProgramArguments = [ "${tool}/bin/mac-diag" "run" "--slots" (lib.concatStringsSep "," slots) ];
    # ./mac-diag's RUN_EVERY_S. Each run collects only what is due.
    StartInterval = 300;
    # The connector's request_refresh drops a file here; launchd starts a
    # run, which empties it. launchd runs one at a time.
    QueueDirectories = [ "${dir}/queue/fast" ];
    RunAtLoad = true;
    Nice = 10;
    StandardOutPath = collectLog;
    StandardErrorPath = collectLog;
  };

  launchd.daemons.mac-diag-disk.serviceConfig = {
    ProgramArguments = [ "${tool}/bin/mac-diag" "run-disk" ];
    # Each run scans only when ./mac-diag's DISK_EVERY_H is up or asked.
    StartInterval = 1800;
    QueueDirectories = [ "${dir}/queue/disk" ];
    RunAtLoad = true;
    # It walks the Data volume with du, as mac/diagnose's does.
    LowPriorityIO = true;
    Nice = 15;
    StandardOutPath = diskLog;
    StandardErrorPath = diskLog;
  };

  launchd.daemons.mac-diag-mcp.serviceConfig = {
    ProgramArguments = [ "${server}/bin/mac-diag-mcp" ];
    UserName = user;
    GroupName = user;
    EnvironmentVariables = {
      HOME = "/var/empty";
      OAUTH_RESOURCE = resource;
      OAUTH_ALLOWED_CLIENTS = client;
      MAC_DIAG_SNAPSHOT = "${dir}/snapshot.json";
      MAC_DIAG_QUEUE = "${dir}/queue";
      MAC_DIAG_STATE_DIR = stateDir;
      MAC_DIAG_HOSTNAME = hostname;
      MAC_DIAG_CONFIG = settings;
    };
    RunAtLoad = true;
    # It exits 0 when it is not set up yet, and stays down until `mac-diag
    # login` starts it again; any other exit is restarted.
    KeepAlive = { SuccessfulExit = false; };
    ThrottleInterval = 30;
    StandardOutPath = serverLog;
    StandardErrorPath = serverLog;
  };
}
