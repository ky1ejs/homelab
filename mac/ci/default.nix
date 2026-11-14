# iOS CI for Cut on the Mac Studio: phase 2 of the setup plan.
#
# Each job runs in a fresh Tart VM that is deleted afterwards. worker runs two
# runner slots, the most macOS VMs Apple's licence allows at once, each a
# launchd agent that loops: clone the image, boot it on Softnet, take one job
# as a just-in-time runner in cut-watch's `cut-mac` runner group, delete the
# VM (./runner). A heartbeat pings Healthchecks.io's "Mac CI" check, which
# Cut's workflows read to send iOS jobs here or to GitHub's runners
# (./heartbeat).
#
# The VMs run as worker, in worker's login session: Apple's Virtualization
# framework does not run in a launchd daemon. After a restart they start once
# worker logs in (docs/mac-studio.md, "Logging worker in"); until then the
# heartbeat is silent and Cut's jobs go to GitHub.
#
# What this does NOT do, and why (docs/mac-studio.md, "iOS CI", has the steps):
# - Make the GitHub App, its key, or the runner group. They are GitHub
#   settings, made once by hand. The key is a file in worker's home.
# - Build the VM image. `build-image`, run by hand in worker-shell, pulls about
#   60 GB; an apply should not.
# - Hold the heartbeat's ping URL. It is a secret, kept in worker's home.
{ config, lib, pkgs, nixpkgs-unstable, ... }:

let
  user = "worker";
  home = "/Users/${user}";

  # From nixpkgs-unstable: 26.05's Softnet is 0.7.1, which predates its
  # default of letting a VM reach only globally routable addresses, so a job
  # could reach the LAN and the tailnet. Tart comes from the same set, to
  # match. Both are source-available (FSL), which nixpkgs counts as unfree.
  unstable = import nixpkgs-unstable {
    inherit (pkgs.stdenv.hostPlatform) system;
    config.allowUnfreePredicate = p: builtins.elem (lib.getName p) [ "tart" "softnet" ];
  };
  inherit (unstable) tart softnet;

  ci = {
    org = "cut-watch";
    group = "cut-mac";
    # Cut's workflows run iOS jobs on { group: cut-mac, labels: cut-mac }.
    label = "cut-mac";
    image = "cut-ci";
    slots = 2;
    # Per VM. Two of them fit the memory budget in the decision record: 16 GB
    # for CI out of 64.
    cpu = 6;
    memoryMB = 8 * 1024;
    # Below this the runners stop taking jobs and the heartbeat fails, which
    # sends Cut's jobs to GitHub. A job writes about 20 GB to its clone.
    minFreeGB = 50;
    # Longer than any of Cut's iOS workflow timeouts (40 minutes for a deploy).
    maxJobSeconds = 2 * 60 * 60;
  };

  # The versions Cut's .github/actions/ios-setup and workflows use, baked into
  # the image so jobs skip those downloads. A job still works when they drift:
  # ios-setup then downloads its own.
  xcodeVersion = lib.fileContents ../xcode-version;
  xcodegenVersion = "2.44.1";
  apolloCliVersion = "2.4.0";

  # Softnet's store path comes first so Tart runs that file, the one the
  # sudo rule below names. Found through /run/current-system/sw/bin it is a
  # symlink, and sudo does not match a symlink to the file it points at.
  path = lib.concatStringsSep ":" [
    "${softnet}/bin"
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];

  logFile = name: "${home}/Library/Logs/ci-${name}.log";

  ciEnv = {
    PATH = path;
    CI_ORG = ci.org;
    CI_GROUP = ci.group;
    CI_LABEL = ci.label;
    CI_IMAGE = ci.image;
    CI_SLOTS = toString ci.slots;
    CI_CPU = toString ci.cpu;
    CI_MEMORY_MB = toString ci.memoryMB;
    CI_MIN_FREE_GB = toString ci.minFreeGB;
    CI_MAX_JOB_SECONDS = toString ci.maxJobSeconds;
  };

  runnerAgent = slot: {
    Label = "dev.kylejs.ci-runner-${toString slot}";
    ProgramArguments = [ "${./runner}" (toString slot) ];
    EnvironmentVariables = ciEnv;
    StandardOutPath = logFile "runner-${toString slot}";
    StandardErrorPath = logFile "runner-${toString slot}";
    RunAtLoad = true;
    # One job per run: launchd starts the next as soon as it exits.
    KeepAlive = true;
    ThrottleInterval = 10;
    # Stopping a slot stops its VM; give it time to delete the VM and the
    # runner registration.
    ExitTimeOut = 60;
    ProcessType = "Interactive";
    LimitLoadToSessionType = "Aqua";
  };

  heartbeatAgent = {
    Label = "dev.kylejs.ci-heartbeat";
    ProgramArguments = [ "${./heartbeat}" ];
    EnvironmentVariables = ciEnv;
    StandardOutPath = logFile "heartbeat";
    StandardErrorPath = logFile "heartbeat";
    RunAtLoad = true;
    StartInterval = 300;
    LimitLoadToSessionType = "Aqua";
  };

  buildImage = pkgs.writeShellScriptBin "build-image" ''
    export CI_IMAGE=${lib.escapeShellArg ci.image}
    export CI_CPU=${toString ci.cpu}
    export CI_MEMORY_MB=${toString ci.memoryMB}
    export XCODE_VERSION=${lib.escapeShellArg xcodeVersion}
    export XCODEGEN_VERSION=${lib.escapeShellArg xcodegenVersion}
    export APOLLO_CLI_VERSION=${lib.escapeShellArg apolloCliVersion}
    export CI_IMAGE_SETUP=${./image-setup}
    exec ${./build-image} "$@"
  '';
in
{
  homelab.worker.agents = (map runnerAgent (lib.range 1 ci.slots)) ++ [ heartbeatAgent ];

  environment.systemPackages = [ tart softnet buildImage ];

  # Softnet needs root to make its vmnet interface, then drops to worker. Tart
  # checks for it with `sudo --non-interactive softnet --help`, and Softnet
  # re-runs itself under sudo. The Nix store cannot hold the setuid bit Tart
  # would otherwise set, so worker may run this one binary as root, with no
  # password and no other environment than the two variables it passes on.
  # The rule names the store path, which the runner's PATH puts first.
  security.sudo.extraConfig = ''
    Defaults!${softnet}/bin/softnet env_keep += "SENTRY_DSN CIRRUS_SENTRY_TAGS"
    ${user} ALL = (root) NOPASSWD: ${softnet}/bin/softnet
  '';
}
