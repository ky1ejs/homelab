# Claude Remote Control on the Mac Studio: phase 3 of the setup plan.
#
# Everything here runs as `worker`, a standard user with no admin rights, in
# worker's own login session. After a restart, the FileVault unlock stops at
# the login window: these servers start once worker logs in, with Screen
# Sharing (docs/mac-studio.md, "Logging worker in").
#
# What this sets, on every apply once worker exists:
# - a `claude remote-control` server for each repo in `repos` below (Cut, the
#   one with an iOS app), each a launchd agent in worker's
#   ~/Library/LaunchAgents. A session gets its own git worktree;
# - `sync`, every 5 minutes: clones each listed repo if it is missing and
#   fetches it, so new worktrees branch from a recent main (./sync-repos);
# - the "any repo" server in ~/code/any, for every other repo: each session
#   starts in an empty folder of its own and clones what it needs
#   (./any-session, ./any-AGENTS.md);
# - `sweep`, daily: a session folder either server kept, with nothing in it
#   changed for 14 days, has its unsaved work saved to ~/code/salvage and is
#   deleted (./sweep-sessions);
# - the heartbeat, every 5 minutes; `relogin`, for a lapsed sign-in; and
#   `worker-shell`, a shell in worker's session for the one-time setup;
# - xcbeautify and xcodegen on PATH, for Cut's ios/scripts;
# - git config for the servers and worker-shell (Git LFS, and the author
#   name and email), through git's GIT_CONFIG_* environment variables rather
#   than a dotfile (gitEnv below).
#
# No Bash sandbox: it blocks CoreSimulator, which Cut's builds and tests need.
# worker being a standard user, with no keys on the host, is the boundary.
#
# What this does NOT do, and why (docs/mac-studio.md has the steps):
# - Create worker. FileVault takes only a user created with an admin password,
#   and nix-darwin's own user creation cannot do that.
# - Sign in to claude.ai, trust each folder, or accept Remote Control's
#   one-time prompt. Each is an interactive prompt, answered once by hand.
# - Hold GitHub tokens. They are one-time, by hand: files in worker's home that ./github-token hands to git and gh: one for
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

  # git config for git run by the servers, their sessions and worker-shell.
  # worker has no dotfiles, and Nix's git reads its system config from its
  # store path, not /etc/gitconfig, so these go in git's GIT_CONFIG_COUNT,
  # GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n variables. They add to
  # ~/.gitconfig, which keeps the github-token credential helper, and the
  # config stays here rather than in a file in worker's home.
  gitConfig = {
    # Git LFS's filter. Without it worktrees check out LFS pointer files, and
    # Cut's asset catalog fails with "AppIcon did not have any applicable
    # content". git-lfs is installed (mac/hosts/mac-studio.nix), but
    # `git lfs install` would write ~/.gitconfig.
    "filter.lfs.process" = "git-lfs filter-process";
    "filter.lfs.smudge" = "git-lfs smudge -- %f";
    "filter.lfs.clean" = "git-lfs clean -- %f";
    "filter.lfs.required" = "true";
    # The author and committer of the sessions' commits: Kyle, with the email
    # his own commits use, so GitHub links them to his account. Without these
    # git takes worker's full name and worker@<hostname>, which GitHub shows
    # as "worker" with no account. The agent's Co-Authored-By trailer still
    # marks its commits.
    "user.name" = "Kyle Satti";
    "user.email" = "k@kylejs.dev";
  };
  gitEnv = {
    GIT_CONFIG_COUNT = toString (lib.length (lib.attrNames gitConfig));
  } // lib.listToAttrs (lib.concatLists (lib.imap0 (i: kv: [
    (lib.nameValuePair "GIT_CONFIG_KEY_${toString i}" kv.name)
    (lib.nameValuePair "GIT_CONFIG_VALUE_${toString i}" kv.value)
  ]) (lib.attrsToList gitConfig)));

  # launchd agents start with a bare PATH.
  path = lib.concatStringsSep ":" [
    "/run/current-system/sw/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  ];

  # A server of its own for each repo here, cloned to ~/code/<name> and
  # fetched every 5 minutes (syncAgent). Claude Code 2.1.322 serves one folder
  # per server, and a session started in the repo's own folder loads all of
  # its project settings: hooks, permission rules, plugins and MCP servers.
  # Every other repo goes through the "any repo" server (anyServer). List a
  # repo here when its sessions need those settings or it is used often.
  #
  # Capacity is the most sessions a server holds at once, idle ones included
  # until they are archived. An idle session costs little; a build with a
  # simulator is about 6 to 10 GB, and the heartbeat reports swap over 8 GB.
  repos = {
    cut = { repo = "cut-watch/cut"; capacity = 10; };
  };

  # The "any repo" server: ~/code/any is not a git repo, so its sessions get
  # their folders from ./any-session through the WorktreeCreate and
  # WorktreeRemove hooks in ~/code/any/.claude/settings.json.
  anyServer = { dir = "${code}/any"; capacity = 5; };

  logFile = name: "${home}/Library/Logs/claude-rc-${name}.log";

  # name: the label and log name. title: what the Claude app lists.
  serverAgent = { name, title, dir, capacity }: {
    Label = "dev.kylejs.claude-rc.${name}";
    ProgramArguments = [
      claude
      "remote-control"
      "--name"
      title
      "--spawn"
      "worktree"
      "--capacity"
      (toString capacity)
      "--permission-mode"
      "auto"
      # No session in the main folder: sessions start from the app, each in
      # its own worktree or session folder.
      "--no-create-session-in-dir"
    ];
    WorkingDirectory = dir;
    EnvironmentVariables = gitEnv // { PATH = path; };
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
    EnvironmentVariables = gitEnv // {
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

  syncAgent = {
    Label = "dev.kylejs.claude-sync";
    ProgramArguments = [ "${./sync-repos}" ]
      ++ lib.mapAttrsToList (name: r: "${name}=${r.repo}") repos;
    EnvironmentVariables = gitEnv // { PATH = path; };
    StandardOutPath = logFile "sync";
    StandardErrorPath = logFile "sync";
    RunAtLoad = true;
    StartInterval = 300;
    LimitLoadToSessionType = "Aqua";
  };

  # At load, which is each login and each apply that changes it, and at 04:00
  # daily. The heartbeat counts its age from the restart at the latest, so
  # without RunAtLoad a new install would read as late until 04:00.
  sweepAgent = {
    Label = "dev.kylejs.claude-sweep";
    ProgramArguments = [ "${./sweep-sessions}" ] ++ lib.attrNames repos;
    EnvironmentVariables = gitEnv // { PATH = path; };
    StandardOutPath = logFile "sweep";
    StandardErrorPath = logFile "sweep";
    RunAtLoad = true;
    StartCalendarInterval = { Hour = 4; Minute = 0; };
    LimitLoadToSessionType = "Aqua";
  };

  repoAgents = lib.mapAttrsToList (name: r: serverAgent {
    inherit name;
    title = "mac-studio ${name}";
    dir = "${code}/${name}";
    inherit (r) capacity;
  }) repos;

  anyAgent = serverAgent {
    name = "any";
    title = "mac-studio any repo";
    inherit (anyServer) dir capacity;
  };

  agents = repoAgents ++ config.homelab.worker.agents ++ [
    anyAgent
    syncAgent
    sweepAgent
    heartbeatAgent
    (sessionAgent "relogin")
    (sessionAgent "shell")
  ];

  anySession = pkgs.runCommand "any-session" { } ''
    install -Dm755 ${./any-session} $out/bin/any-session
    substituteInPlace $out/bin/any-session \
      --replace-fail '@jq@' '${pkgs.jq}/bin/jq'
  '';

  # Project settings for the "any repo" server's folder only.
  anySettings = pkgs.writeText "any-settings.json" (builtins.toJSON {
    hooks = {
      WorktreeCreate = [{
        hooks = [{ type = "command"; command = "${anySession}/bin/any-session create"; }];
      }];
      WorktreeRemove = [{
        hooks = [{ type = "command"; command = "${anySession}/bin/any-session remove"; }];
      }];
    };
  });

  plist = a: pkgs.writeText "${a.Label}.plist" (lib.generators.toPlist { escape = true; } a);

  # `relogin` and `worker-shell`: one script, which reads the name it was run
  # by.
  workerAttach = pkgs.runCommand "worker-attach" { } ''
    install -Dm755 ${./worker-attach} $out/bin/worker-attach
    ln -s worker-attach $out/bin/relogin
    ln -s worker-attach $out/bin/worker-shell
  '';

  q = lib.escapeShellArg;

  drain = config.homelab.worker.drain;

  installAgent = a: ''
    install_agent ${q a.Label} ${plist a} ${q (drain.commands.${a.Label} or "")}
  '';
in
{
  # Launchd agents other modules add for worker (mac/ci), installed and
  # removed by the activation script below along with this module's own.
  options.homelab.worker.agents = lib.mkOption {
    type = lib.types.listOf (lib.types.attrsOf lib.types.anything);
    default = [ ];
    description = "launchd agent plists, as attribute sets, to install in worker's ~/Library/LaunchAgents. Labels start dev.kylejs.";
  };

  # Agents whose restart can kill work (mac/ci's runner slots). Before an
  # apply restarts one, the activation script runs its command as worker:
  # `<command> wait <label> <minutes>` exits 0 once the restart loses
  # nothing, or 1 after the minutes, when the agent keeps its old plist; and
  # `<command> release <label>` after the restart.
  options.homelab.worker.drain = {
    commands = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      description = "Drain command per agent label, run before an apply restarts that agent.";
    };
    minutes = lib.mkOption {
      type = lib.types.ints.positive;
      description = "How long an apply waits for an agent to drain. mac/apply --wait-minutes overrides it, and --no-wait skips the wait.";
    };
  };

  config = {
    assertions = [{
      assertion = !(repos ? any);
      message = "mac/claude: \"any\" is the any-repo server's name, so no repo in `repos` can use it";
    }];

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
        as_worker mkdir -p ${home}/Library/LaunchAgents ${home}/Library/Logs ${code} \
          ${anyServer.dir}/.claude ${anyServer.dir}/sessions

        # The "any repo" server's folder: its hooks and its sessions' guidance.
        install_file() {
          if cmp -s "$1" "$2"; then return; fi
          echo "installing $2 for ${user}" >&2
          as_worker install -m 644 "$1" "$2"
        }
        install_file ${anySettings} ${anyServer.dir}/.claude/settings.json
        install_file ${./any-AGENTS.md} ${anyServer.dir}/AGENTS.md

        # Without a login session there is no gui domain; worker's next login
        # loads the plists instead.
        has_session=false
        if launchctl print "gui/$uid" >/dev/null 2>&1; then has_session=true; fi

        # How long to wait for a busy agent before restarting it: mac/apply
        # writes this file for --no-wait (0) and --wait-minutes, and removes
        # it otherwise. The activation script runs with an empty environment,
        # so a file is the way in. Only root can write /var/run.
        drain_minutes=${toString drain.minutes}
        drain_minutes_file=/var/run/homelab-apply-drain-minutes
        if [ -f "$drain_minutes_file" ]; then
          drain_minutes_set=$(cat "$drain_minutes_file")
          rm -f "$drain_minutes_file"
          case "$drain_minutes_set" in
            "" | *[!0-9]*) echo "warning: ignoring $drain_minutes_file, which holds no number of minutes" >&2 ;;
            *) drain_minutes=$drain_minutes_set ;;
          esac
        fi
        kept_old=""

        install_agent() {
          local dst=${home}/Library/LaunchAgents/$1.plist drain_cmd="$3" drained=false
          if cmp -s "$2" "$dst"; then return; fi
          # A loaded agent with a drain command gets to finish its work
          # first. If it can't in time, it keeps its old plist, so the next
          # apply tries again.
          if $has_session && [ -n "$drain_cmd" ] && launchctl print "gui/$uid/$1" >/dev/null 2>&1; then
            if [ "$drain_minutes" -eq 0 ]; then
              echo "restarting $1 without waiting for its work (--no-wait)" >&2
            elif as_worker "$drain_cmd" wait "$1" "$drain_minutes"; then
              drained=true
            else
              kept_old="$kept_old $1"
              return
            fi
          fi
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
            # The old agent is gone; the new one may take work.
            if $drained; then
              as_worker "$drain_cmd" release "$1" || echo "warning: could not release $1; run: sudo -u ${user} $drain_cmd release $1" >&2
            fi
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

        # An agent taken out of the lists above stops and its plist goes.
        # nix-darwin shellchecks this script, which rejects a `case` on a
        # constant word (SC2194), so the labels go in a variable first.
        keep=" ${lib.concatMapStringsSep " " (a: a.Label) agents} "
        for f in ${home}/Library/LaunchAgents/dev.kylejs.*.plist; do
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

        if [ -n "$kept_old" ]; then
          echo "warning: these kept their old config, still busy after $drain_minutes minutes:$kept_old" >&2
          echo "warning: run mac/apply again once they finish, or mac/apply --no-wait to restart them now and kill their work" >&2
        fi
      fi
    '';
  };
}
