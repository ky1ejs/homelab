# Linux containers on the Mac Studio: phase 4 of the setup plan.
#
# Colima runs one Linux VM as worker, in worker's login session, and serves
# the Docker API from it. worker's Claude sessions use it for the services a
# repo runs with Docker Compose: Cut's Postgres, Redis and S3, shared by all
# its worktrees (Cut's scripts/bootstrap). Without it, an agent with no
# Docker installed Postgres and Redis on the host on 2026-11-13, which every
# session then shared, at whatever version that agent picked.
#
# Nothing moves off the NAS in this phase.
#
# What this sets, on every apply once worker exists:
# - colima and the docker client (with its compose and buildx plugins) on
#   PATH;
# - a launchd agent in worker's session that starts the VM at login and
#   stops it when the agent stops (./start);
# - DOCKER_HOST for worker's Claude servers, worker-shell and the heartbeat,
#   which also checks that Docker answers and that no Postgres, Redis or S3
#   server runs on the host (mac/claude/heartbeat).
#
# What this does NOT do: the first start downloads the VM image, about
# 300 MB, at worker's next login after the apply.
{ lib, pkgs, ... }:

let
  user = "worker";
  home = "/Users/${user}";

  # From the decision record's memory budget: 4 GB for Colima, beside two CI
  # VMs at 8 GB and two Claude sessions with simulators. Cut's services use
  # well under 1 GB. The disk grows as images and volumes fill it, up to this
  # size; colima can grow a disk later but not shrink it.
  vm = {
    cpus = 4;
    memoryGiB = 4;
    diskGiB = 60;
  };

  # A container can bind-mount only folders the VM mounts. All of worker's
  # sessions work under ~/code (mac/claude), so that is the one mount, and
  # writable. virtiofs is the fastest type on the vz VM type.
  mounts = [ "${home}/code:w" ];

  # colima's default profile, in its default folder (~/.colima, as no
  # COLIMA_HOME is set). colima also makes a "colima" docker context in
  # ~/.docker and selects it; DOCKER_HOST wins over any context, so a session
  # that selects another still reaches this VM.
  dockerHost = "unix://${home}/.colima/default/docker.sock";

  path = lib.concatStringsSep ":" [
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];

  logFile = "${home}/Library/Logs/colima.log";

  start = pkgs.writeShellApplication {
    name = "colima-start";
    runtimeInputs = [ pkgs.colima ];
    text = builtins.readFile ./start;
  };
in
{
  environment.systemPackages = with pkgs; [ colima docker ];

  homelab.worker.environment.DOCKER_HOST = dockerHost;

  homelab.worker.agents = [{
    Label = "dev.kylejs.colima";
    ProgramArguments = [
      "${start}/bin/colima-start"
      "--vm-type" "vz"
      "--mount-type" "virtiofs"
      "--cpus" (toString vm.cpus)
      "--memory" (toString vm.memoryGiB)
      "--disk" (toString vm.diskGiB)
    ] ++ lib.concatMap (m: [ "--mount" m ]) mounts;
    EnvironmentVariables = { PATH = path; HOME = home; };
    StandardOutPath = logFile;
    StandardErrorPath = logFile;
    RunAtLoad = true;
    KeepAlive = true;
    ThrottleInterval = 30;
    # On SIGTERM colima stops the VM, which takes several seconds; launchd's
    # default 20 before SIGKILL could leave it running. An apply that changes
    # this plist restarts the VM, and with it every session's containers.
    ExitTimeOut = 120;
    LimitLoadToSessionType = "Aqua";
  }];
}
