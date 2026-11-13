# Claude Remote Control on the Mac Studio: phase 3 of the setup plan.
#
# Everything here runs as `agent`, a standard user with no admin rights, in
# agent's own login session. agent does the FileVault unlock, so after a
# restart its session, and these servers, start with no one at the keyboard.
#
# What this sets, on every apply once agent exists:
# - one `claude remote-control` server, for Cut, the one repo with an iOS
#   app: a launchd agent in agent's ~/Library/LaunchAgents;
# - the heartbeat, every 5 minutes; `relogin`, for a lapsed sign-in; and
#   `agent-shell`, a shell in agent's session for the one-time setup.
#
# No Bash sandbox: it blocks CoreSimulator, which Cut's builds and tests need.
# agent being a standard user, with no keys on the host, is the boundary.
#
# What this does NOT do, and why (docs/mac-studio.md has the steps):
# - Create agent. FileVault takes only a user created with an admin password,
#   and nix-darwin's own user creation cannot do that.
# - Sign in to claude.ai, trust each folder, or accept Remote Control's
#   one-time prompt. Each is an interactive prompt, answered once by hand.
# - Clone Cut or sign in to GitHub. Both are one-time, by hand.
# - Hold the heartbeat's ping URL. It is a secret, kept in agent's home.
#
# nix-darwin's launchd.user.agents install only for system.primaryUser
# (kylejs), so the activation script below installs agent's own.
{ config, lib, pkgs, ... }:

let
  user = "agent";
  home = "/Users/${user}";
  code = "${home}/code";

  # Homebrew's claude-code@latest cask puts it here. The stable cask lags the
  # 2.1.317 floor in the decision record, and nixpkgs' claude-code lags
  # further. Switching to a Nix package is this line and the cask below.
  claude = "/opt/homebrew/bin/claude";

  # launchd agents start with a bare PATH.
  path = lib.concatStringsSep ":" [
    "/opt/homebrew/bin"
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
    # would go in agent's ~/.claude/settings.json.
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

  # Started by `relogin` and `agent-shell` (./agent-attach), never on their
  # own. Each opens a tmux session inside agent's login session, where agent's
  # keychain is unlocked, and the command attaches to it over SSH.
  sessionAgent = session: {
    Label = "dev.kylejs.claude-${session}";
    ProgramArguments = [ "${./agent-session}" "start" session ];
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

  # `relogin` and `agent-shell`: one script, which reads the name it was run
  # by.
  agentAttach = pkgs.runCommand "agent-attach" { } ''
    install -Dm755 ${./agent-attach} $out/bin/agent-attach
    ln -s agent-attach $out/bin/relogin
    ln -s agent-attach $out/bin/agent-shell
  '';

  q = lib.escapeShellArg;

  installAgent = a: ''
    install_agent ${q a.Label} ${plist a}
  '';
in
{
  environment.systemPackages = [ agentAttach ];

  homebrew.casks = [ "claude-code@latest" ];

  system.activationScripts.postActivation.text = lib.mkAfter ''
    # Phase 3: agent's Claude Remote Control servers (mac/claude).
    if ! uid=$(id -u ${user} 2>/dev/null); then
      echo "note: no ${user} user yet, so no Claude Remote Control (docs/mac-studio.md, phase 3)" >&2
    else
      as_agent() { sudo --user=${user} -- "$@"; }
      as_agent mkdir -p ${home}/Library/LaunchAgents ${home}/Library/Logs ${code}

      # Without a login session there is no gui domain; agent's next login
      # loads the plists instead.
      has_session=false
      if launchctl print "gui/$uid" >/dev/null 2>&1; then has_session=true; fi

      install_agent() {
        local dst=${home}/Library/LaunchAgents/$1.plist
        if cmp -s "$2" "$dst"; then return; fi
        echo "installing $1 for ${user}" >&2
        as_agent install -m 644 "$2" "$dst"
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
