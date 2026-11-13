# The Mac Studio's base host: phase 1 of the setup plan.
#
# What is deliberately NOT here, and why (docs/mac-studio.md has the detail):
# - FileVault and auto-login. FileVault is on, which turns auto-login off, and
#   neither can be set safely from a config: both need a password. Unlocking
#   FileVault over SSH stops at the login window, so after a restart `worker`
#   logs in with Screen Sharing for its Claude Remote Control servers to start.
# - Creating the `worker` user. FileVault takes only a user created with an
#   admin password. mac/claude, imported below, sets everything worker runs.
# - Signing in to an Apple Account, and turning its password sync off, which
#   keeps the Studio off Trusted Wi-Fi. Both are by hand: nix-darwin does
#   neither.
# - Remote Management (Screen Sharing and Apple Remote Desktop). macOS only
#   lets it be turned on in System Settings, and it logs in with the account
#   password, not an SSH key.
# - The screen lock's delay. Since macOS 13 it ignores the askForPassword
#   default and takes the setting only with the user's password
#   (`sysadminctl -screenLock`) or from MDM, so it is a day-one command, for
#   kylejs and again for worker. What is here is the lock-screen daemon below,
#   which relies on it.
{ config, lib, pkgs, ... }:

let
  admin = "kylejs";

  # One public key per line. Comment lines are fine: sshd skips them.
  keyFile = ../keys/kylejs.pub;
  keyLines = builtins.filter
    (l: l != "" && !(lib.hasPrefix "#" l))
    (lib.splitString "\n" (builtins.readFile keyFile));
in
{
  # Phase 3: Claude Remote Control, run by `worker`.
  imports = [ ../claude ];

  nixpkgs.hostPlatform = "aarch64-darwin";

  # Set once, on the first switch. Changing it later changes defaults under
  # running services; read the nix-darwin changelog before you do.
  system.stateVersion = 7;

  # nix-darwin applies per-user settings (defaults, Homebrew) as this user.
  system.primaryUser = admin;

  networking = {
    hostName = "mac-studio";
    computerName = "mac-studio";
    localHostName = "mac-studio";
  };

  time.timeZone = "America/New_York";

  # nix-darwin manages an account here only through `users.knownUsers`, and
  # this one is not in it on purpose: Setup Assistant created it, and a typo in
  # a uid would have nix-darwin edit the only admin. This entry is just enough
  # for the SSH keys.
  users.users.${admin} = {
    home = "/Users/${admin}";
    openssh.authorizedKeys.keyFiles = [ keyFile ];
  };

  warnings = lib.optional (keyLines == [ ]) ''
    mac/keys/kylejs.pub has no public keys, and SSH password login is off.
    Nobody can SSH in until a key is added. The keyboard and Remote
    Management still work.
  '';

  nix = {
    settings = {
      experimental-features = [ "nix-command" "flakes" ];
      # trusted-users stays at its default, root. A trusted user can hand the
      # daemon options it runs as root (a post-build hook, sandbox off,
      # unsigned paths), so adding kylejs would make every process running as
      # kylejs root without sudo. A binary cache goes in substituters and
      # trusted-public-keys here instead.
    };
    optimise.automatic = true;
    gc = {
      automatic = true;
      options = "--delete-older-than 30d";
    };
  };

  # The tools kylejs-macOS-bootstrap's dotfiles expect (starship, git-lfs).
  # The dotfiles themselves come from that repo, not from here. Node and Bun
  # are for worker's Cut sessions: Cut's backend and web builds use Bun.
  #
  # Command-line tools only: GUI apps are Homebrew casks (below). A package
  # here with an app bundle would land in a separate /Applications/Nix Apps
  # folder, and nix-darwin's check of that folder then stops every apply over
  # SSH, Tailscale SSH included.
  environment.systemPackages = with pkgs; [
    bun
    # Low priority: mac/claude's github-token provides bin/gh, which picks
    # worker's token and runs this one. This still gives gh's completions.
    (lib.lowPrio gh)
    git
    git-lfs
    jq
    nodejs_24
    ripgrep
    starship
    tmux
    # Installs the Xcode pinned in mac/xcode-version (mac/bootstrap, step 5).
    # nixpkgs wraps it with aria2, which makes the download several times
    # faster.
    xcodes
  ];

  # Puts fish in /etc/shells. Making it the login shell is one `chsh` on day
  # one (docs/mac-studio.md): nix-darwin only sets shells for users it manages.
  programs.fish.enable = true;
  environment.shells = [ pkgs.fish ];

  # Remote Login. Keys only: the password stays for sudo and the keyboard.
  # Over the tailnet, Tailscale SSH answers port 22 instead, so this sshd
  # serves LAN SSH.
  # FileVault's unlock over SSH runs before the data volume holding this file
  # is unlocked, so these lines do not affect it: it takes the password
  # (tested 2026-11-13).
  services.openssh.enable = true;
  # sshd takes the first value it reads for each setting, and reads
  # sshd_config.d in name order, so this file starts with 000 to come before
  # macOS's own 100-macos.conf and nix-darwin's 100-nix-darwin.conf.
  environment.etc."ssh/sshd_config.d/000-homelab.conf".text = ''
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    PermitRootLogin no
  '';

  # tailscaled as a launchd daemon, so the Studio is on the tailnet before
  # anyone logs in. Joining is one manual `tailscale up --ssh` with a tagged
  # auth key (docs/mac-studio.md); the key never goes in this repo.
  services.tailscale.enable = true;
  # nix-darwin sets only RunAtLoad, so a crashed tailscaled would stay down
  # until a reboot, and away from home the tailnet is the only way in.
  # launchd restarts it, no more than once every 10 seconds.
  launchd.daemons.tailscaled.serviceConfig = {
    KeepAlive = true;
    ThrottleInterval = 10;
  };

  power = {
    sleep = {
      computer = "never";
      harddisk = "never";
      display = 10;
    };
    restartAfterPowerFailure = true;
    restartAfterFreeze = true;
  };

  # The settings nix-darwin has no option for. `standby` and `autopoweroff`
  # are not on every Mac, so each runs on its own and a refusal is a warning,
  # not a failed switch.
  system.activationScripts.postActivation.text = ''
    for setting in "standby 0" "autopoweroff 0" "powernap 0" "tcpkeepalive 1" "womp 1"; do
      # shellcheck disable=SC2086
      if ! pmset -a $setting >/dev/null 2>&1; then
        echo "warning: pmset -a $setting was refused on this Mac" >&2
      fi
    done
  '';

  # No automatic macOS updates: an Xcode toolchain bump mid-week breaks CI, so
  # updates are a monthly chore. Security responses and XProtect data still
  # install on their own, because they don't touch Xcode.
  system.defaults.SoftwareUpdate.AutomaticallyInstallMacOSUpdates = false;
  system.defaults.CustomSystemPreferences."/Library/Preferences/com.apple.SoftwareUpdate" = {
    AutomaticCheckEnabled = true;
    AutomaticDownload = false;
    CriticalUpdateInstall = true;
    ConfigDataInstall = true;
  };

  # No guest account: the Studio has a display and keyboard attached.
  system.defaults.loginwindow.GuestEnabled = false;

  # Locks the screen when a Screen Sharing or ARD session ends, by putting the
  # display to sleep. macOS doesn't lock then, so the desktop would stay open
  # to anyone at the Studio until the display sleeps on its own. After the
  # FileVault unlock the Studio is at the login window, which needs no lock.
  # mac/lock-screen says when it locks. It runs as root so it covers whichever
  # user is at the console. It only locks for a user whose
  # `sysadminctl -screenLock` is immediate (mac/bootstrap sets kylejs's).
  launchd.daemons.lock-screen.serviceConfig = {
    ProgramArguments = [ "/bin/bash" "${../lock-screen}" ];
    RunAtLoad = true;
    KeepAlive = true;
    ThrottleInterval = 10;
    StandardOutPath = "/var/log/lock-screen.log";
    StandardErrorPath = "/var/log/lock-screen.log";
  };

  # The Dock, reset to this list on every apply: an app dragged in by hand is
  # gone at the next one. Finder is always first and is not listed. Ghostty
  # comes from the Homebrew cask below. /Applications/Xcode.app is the link
  # mac/bootstrap makes to the pinned Xcode, so the tile shows a question mark
  # until the bootstrap's Xcode step has run.
  system.defaults.dock = {
    persistent-apps = [
      "/Applications/Safari.app"
      "/Applications/Ghostty.app"
      "/System/Applications/Utilities/Activity Monitor.app"
      "/System/Applications/System Settings.app"
      "/Applications/Xcode.app"
    ];
    show-recents = false;
  };

  # The wallpaper, mac/wallpaper/mac-studio-dark.png (its README says how it
  # is made). A wallpaper is a per-user setting that only the user's own GUI
  # session can change, so desktoppr runs as a user launchd agent: at each
  # login, and at an apply that changes the picture. A picture picked by hand
  # lasts until the next login.
  launchd.user.agents.wallpaper.serviceConfig = {
    ProgramArguments = [
      "${pkgs.desktoppr}/bin/desktoppr"
      "${../wallpaper/mac-studio-dark.png}"
    ];
    RunAtLoad = true;
    LimitLoadToSessionType = "Aqua";
  };

  # Homebrew holds the GUI apps, as casks in /Applications, where Finder and
  # Spotlight look; everything else comes from Nix. Homebrew itself is
  # installed by mac/bootstrap. Anything installed with `brew` by hand and not
  # listed here is removed at the next apply, so add it here instead. No
  # auto-update or upgrade on apply: versions move with the monthly chore, not
  # mid-week.
  homebrew = {
    enable = true;
    onActivation = {
      autoUpdate = false;
      upgrade = false;
      cleanup = "uninstall";
    };
    casks = [ "ghostty" ];
  };
}
