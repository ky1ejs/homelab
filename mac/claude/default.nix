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
#   `worker-shell`, a shell in worker's session for the one-time setup;
# - xcbeautify and xcodegen on PATH, for Cut's ios/scripts;
# - Git LFS for the servers and worker-shell, through git's GIT_CONFIG_*
#   environment variables rather than a dotfile (gitLfsEnv below).
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

  # worker's GitHub tokens, one per owner (./github-token): git's credential
  # helper, and the Studio's `gh`. It is the only gh on PATH because login
  # shells, and the shells Claude's sessions start, rebuild PATH, so a link
  # first in a launchd PATH alone is lost. With no token files, as for
  # kylejs, it runs the plain gh.
  githubToken = pkgs.runCommand "github-token" { } ''
    install -Dm755 ${./github-token} $out/bin/github-token
    substituteInPlace $out/bin/github-token \
      --replace-fail '@realGh@' '${pkgs.gh}/bin/gh'
    ln -s github-token $out/bin/gh
  '';

  # Git LFS's filter, for git run by the servers, their sessions and
  # worker-shell. Without it worktrees check out LFS pointer files, and Cut's
  # asset catalog fails with "AppIcon did not have any applicable content".
  # git-lfs is installed (mac/hosts/mac-studio.nix), but `git lfs install`
  # would write ~/.gitconfig, and Nix's git reads its system config from its
  # store path, not /etc/gitconfig. worker has no dotfiles. GIT_CONFIG_COUNT
  # adds these keys on top of ~/.gitconfig, which keeps the github-token
  # credential helper, and the config stays here rather than in a file in
  # worker's home.
  gitLfsEnv = {
    GIT_CONFIG_COUNT = "4";
    GIT_CONFIG_KEY_0 = "filter.lfs.process";
    GIT_CONFIG_VALUE_0 = "git-lfs filter-process";
    GIT_CONFIG_KEY_1 = "filter.lfs.smudge";
    GIT_CONFIG_VALUE_1 = "git-lfs smudge -- %f";
    GIT_CONFIG_KEY_2 = "filter.lfs.clean";
    GIT_CONFIG_VALUE_2 = "git-lfs clean -- %f";
    GIT_CONFIG_KEY_3 = "filter.lfs.required";
    GIT_CONFIG_VALUE_3 = "true";
  };

  # launchd agents start with a bare PATH.
  path = lib.concatStringsSep ":" [
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];

  # Servers only for repos with an iOS app, the work that needs this Mac.
  # Each session gets its own git worktree. The server name is what the
  # Claude app lists. Capacity is the most sessions a server holds at once,
  # idle ones included until they are archived. An idle session costs little;
  # a build with a simulator is about 6 to 10 GB, and the heartbeat reports
  # swap over 8 GB.
  servers = {
    cut = { spawn = "worktree"; capacity = 10; };
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
    EnvironmentVariables = gitLfsEnv // { PATH = path; };
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
    EnvironmentVariables = gitLfsEnv // {
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
  #
  # xcbeautify and xcodegen are for Cut's ios/scripts: build-app.sh stops
  # without xcbeautify, and bootstrap.sh otherwise installs xcodegen into
  # /usr/local, which worker cannot write. Cut pins xcodegen exactly
  # (XCODEGEN_REQUIRED_VERSION in its ios/scripts/bootstrap.sh, 2.44.1 on
  # 2026-11-13), and mac/flake.lock's nixpkgs has that version (xcodegen
  # 2.44.1, xcbeautify 3.2.1). Recheck the match at the monthly flake update.
  environment.systemPackages = [ workerAttach claudeCode githubToken ]
    ++ (with pkgs; [ xcbeautify xcodegen ]);

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
          # bootout returns before the job has exited, and a Remote Control
          # server takes seconds to close its sessions. Loading it again
          # before then fails, which left the server stopped on 2026-11-13.
          # Wait up to 60 seconds for it to go, then load, retrying.
          local waited=0
          while launchctl print "gui/$uid/$1" >/dev/null 2>&1 && [ "$waited" -lt 60 ]; do
            sleep 1
            waited=$((waited + 1))
          done
          local tries=0
          until launchctl bootstrap "gui/$uid" "$dst" 2>/dev/null; do
            tries=$((tries + 1))
            if [ "$tries" -ge 5 ]; then
              echo "warning: launchctl could not load $1; run: sudo launchctl bootstrap gui/$uid $dst" >&2
              break
            fi
            sleep 2
          done
        fi
      }
      ${lib.concatMapStrings installAgent agents}

      # A server taken out of the list above stops and its plist goes.
      # nix-darwin shellchecks this script, which rejects a `case` on a
      # constant word (SC2194), so the labels go in a variable first.
      keep=" ${lib.concatMapStringsSep " " (a: a.Label) agents} "
      for f in ${home}/Library/LaunchAgents/dev.kylejs.claude-*.plist; do
        [ -e "$f" ] || continue
        label=$(basename "$f" .plist)
        case "$keep" in
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
