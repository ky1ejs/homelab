# Claude Remote Control on the Mac Studio: phase 3 of the setup plan.
#
# Everything here runs as `worker`, a standard user with no admin rights, in
# worker's own login session. worker does the FileVault unlock, so after a
# restart its session, and these servers, start with no one at the keyboard.
#
# What this sets, on every apply once worker exists:
# - one `claude remote-control` server, for Cut, the one repo with an iOS
#   app: a launchd agent in worker's ~/Library/LaunchAgents;
# - the heartbeat, every 5 minutes; `relogin`, for a lapsed sign-in; and
#   `worker-shell`, a shell in worker's session for the one-time setup.
#
# No Bash sandbox: it blocks CoreSimulator, which Cut's builds and tests need.
# worker being a standard user, with no keys on the host, is the boundary.
#
# What this does NOT do, and why (docs/mac-studio.md has the steps):
# - Create worker. FileVault takes only a user created with an admin password,
#   and nix-darwin's own user creation cannot do that.
# - Sign in to claude.ai, trust each folder, or accept Remote Control's
#   one-time prompt. Each is an interactive prompt, answered once by hand.
# - Clone Cut or hold GitHub tokens. Both are one-time, by hand. The tokens
#   are files in worker's home that ./github-token hands to git and gh: one for
#   cut-watch, and optionally a read-only one for all ky1ejs repos and a
#   read-write one for picked ones (docs/mac-studio.md says why no more).
# - Hold the heartbeat's ping URL. It is a secret, kept in worker's home.
#
# nix-darwin's launchd.user.agents install only for system.primaryUser
# (kylejs), so the activation script below installs worker's own.
{ config, lib, pkgs, nixpkgs-unstable, ... }:

let
  user = "worker";
  home = "/Users/${user}";
  code = "${home}/code";

  # From nixpkgs-unstable: 26.05's claude-code is older than the 2.1.317
  # floor in the decision record. It is unfree, so only it is allowed.
  # mac/flake.lock pins it; the monthly flake update moves it.
  unstable = import nixpkgs-unstable {
    inherit (pkgs.stdenv.hostPlatform) system;
    config.allowUnfreePredicate = p: lib.getName p == "claude-code";
  };
  claudeCode = unstable.claude-code;
  claude = "${claudeCode}/bin/claude";

  # worker's GitHub tokens, one per owner (./github-token). On PATH for the
  # git credential helper; its `gh` link goes first in worker's own PATH only,
  # so kylejs's gh is the real one.
  githubToken = pkgs.runCommand "github-token" { } ''
    install -Dm755 ${./github-token} $out/bin/github-token
  '';
  ghForWorker = pkgs.runCommand "gh-for-worker" { } ''
    mkdir -p $out/bin
    ln -s ${githubToken}/bin/github-token $out/bin/gh
  '';

  # launchd agents start with a bare PATH.
  path = lib.concatStringsSep ":" [
    "${ghForWorker}/bin"
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];

  # Servers only for repos with an iOS app, the work that needs this Mac.
  # Each session gets its own git worktree. The server name is what the
  # Claude app lists. Capacity is the most sessions a server runs at once.
  servers = {
    cut = { spawn = "worktree"; capacity = 2; };
  };

  logFile = name: "${home}/Library/Logs/claude-rc-${name}.log";

  serverAgent = name: s: {
    Label = "dev.kylejs.claude-rc.${name}";
    # `claude remote-control` refuses `--settings`, so any session settings
    # would go in worker's ~/.claude/settings.json.
    ProgramArguments = [
      claude
      "remote-control"
      "--name"
      "mac-studio ${name}"
      "--spawn"
      s.spawn
      "--capacity"
      (toString s.capacity)
      "--permission-mode"
      "auto"
      # No session in the main checkout: sessions start from the app, each
      # in its own worktree. The main checkout is what worktrees come from.
      "--no-create-session-in-dir"
    ];
    WorkingDirectory = "${code}/${name}";
    EnvironmentVariables.PATH = path;
    StandardOutPath = logFile name;
    StandardErrorPath = logFile name;
    RunAtLoad = true;
    # Restart it whenever it exits, no more than once every 30 seconds. An
    # untrusted folder or a lapsed sign-in makes it exit at once; the
    # heartbeat reads that from the log.
    KeepAlive = true;
    ThrottleInterval = 30;
    ProcessType = "Interactive";
    LimitLoadToSessionType = "Aqua";
  };

  heartbeatAgent = {
    Label = "dev.kylejs.claude-heartbeat";
    ProgramArguments = [ "${./heartbeat}" ];
    EnvironmentVariables.PATH = path;
    StandardOutPath = logFile "heartbeat";
    StandardErrorPath = logFile "heartbeat";
    RunAtLoad = true;
    StartInterval = 300;
    LimitLoadToSessionType = "Aqua";
  };

  # Started by `relogin` and `worker-shell` (./worker-attach), never on their
  # own. Each opens a tmux session inside worker's login session, where worker's
  # keychain is unlocked, and the command attaches to it over SSH.
  sessionAgent = session: {
    Label = "dev.kylejs.claude-${session}";
    ProgramArguments = [ "${./worker-session}" "start" session ];
    EnvironmentVariables = {
      PATH = path;
      CLAUDE_BIN = claude;
    };
    StandardOutPath = logFile session;
    StandardErrorPath = logFile session;
    # tmux detaches its server; without this launchd kills it when the
    # start command exits.
    AbandonProcessGroup = true;
    LimitLoadToSessionType = "Aqua";
  };

  agents = lib.mapAttrsToList serverAgent servers
    ++ [ heartbeatAgent (sessionAgent "relogin") (sessionAgent "shell") ];

  plist = a: pkgs.writeText "${a.Label}.plist" (lib.generators.toPlist { escape = true; } a);

  # `relogin` and `worker-shell`: one script, which reads the name it was run
  # by.
  workerAttach = pkgs.runCommand "worker-attach" { } ''
    install -Dm755 ${./worker-attach} $out/bin/worker-attach
    ln -s worker-attach $out/bin/relogin
    ln -s worker-attach $out/bin/worker-shell
  '';

  q = lib.escapeShellArg;

  installAgent = a: ''
    install_agent ${q a.Label} ${plist a}
  '';
in
{
  # On PATH too, for the one-time setup and `claude auth login` in
  # worker-shell.
  environment.systemPackages = [ workerAttach claudeCode githubToken ];

  system.activationScripts.postActivation.text = lib.mkAfter ''
    # Phase 3: worker's Claude Remote Control servers (mac/claude).
    if ! uid=$(id -u ${user} 2>/dev/null); then
      echo "note: no ${user} user yet, so no Claude Remote Control (docs/mac-studio.md, phase 3)" >&2
    else
      as_worker() { sudo --user=${user} -- "$@"; }
      as_worker mkdir -p ${home}/Library/LaunchAgents ${home}/Library/Logs ${code}

      # Without a login session there is no gui domain; worker's next login
      # loads the plists instead.
      has_session=false
      if launchctl print "gui/$uid" >/dev/null 2>&1; then has_session=true; fi

      install_agent() {
        local dst=${home}/Library/LaunchAgents/$1.plist
        if cmp -s "$2" "$dst"; then return; fi
        echo "installing $1 for ${user}" >&2
        as_worker install -m 644 "$2" "$dst"
        if $has_session; then
          launchctl bootout "gui/$uid/$1" 2>/dev/null || true
          launchctl bootstrap "gui/$uid" "$dst" || echo "warning: launchctl could not load $1" >&2
        fi
      }
      ${lib.concatMapStrings installAgent agents}

      # A server taken out of the list above stops and its plist goes.
      for f in ${home}/Library/LaunchAgents/dev.kylejs.claude-*.plist; do
        [ -e "$f" ] || continue
        label=$(basename "$f" .plist)
        case " ${lib.concatMapStringsSep " " (a: a.Label) agents} " in
          *" $label "*) ;;
          *)
            echo "removing $label for ${user}" >&2
            if $has_session; then launchctl bootout "gui/$uid/$label" 2>/dev/null || true; fi
            rm -f "$f"
            ;;
        esac
      done
    fi
  '';
}
