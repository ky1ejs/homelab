# The Mac Studio's base host: phase 1 of the setup plan.
#
# What is deliberately NOT here, and why (docs/studio.md has the detail):
# - FileVault and auto-login. macOS turns auto-login off when FileVault is on,
#   and neither can be set safely from a config: both need a password. Which
#   one the Studio runs with is still undecided, so both stay manual.
# - The `agent` user. It arrives with Claude Remote Control in phase 3.
# - Signing in to an Apple Account. Undecided, and not something nix-darwin does.
# - Screen Sharing. macOS only lets it be turned on in System Settings.
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
  nixpkgs.hostPlatform = "aarch64-darwin";

  # Set once, on the first switch. Changing it later changes defaults under
  # running services; read the nix-darwin changelog before you do.
  system.stateVersion = 7;

  # nix-darwin applies per-user settings (defaults, Homebrew) as this user.
  system.primaryUser = admin;

  networking = {
    hostName = "studio";
    computerName = "studio";
    localHostName = "studio";
  };

  time.timeZone = "America/New_York";

  # nix-darwin manages an account here only through `users.knownUsers`, and
  # this one is not in it on purpose: Setup Assistant created it, and a typo in
  # a uid would have nix-darwin edit the only admin. This entry is just enough
  # for home-manager and for the SSH keys.
  users.users.${admin} = {
    home = "/Users/${admin}";
    openssh.authorizedKeys.keyFiles = [ keyFile ];
  };

  warnings = lib.optional (keyLines == [ ]) ''
    mac/keys/kylejs.pub has no public keys, and SSH password login is off.
    Nobody can SSH in until a key is added. The keyboard and Screen Sharing
    still work.
  '';

  nix = {
    settings = {
      experimental-features = [ "nix-command" "flakes" ];
      # Who can pass extra substituters and the like to the daemon.
      trusted-users = [ "root" admin ];
    };
    optimise.automatic = true;
    gc = {
      automatic = true;
      options = "--delete-older-than 30d";
    };
  };

  environment.systemPackages = with pkgs; [
    gh
    git
    git-lfs
    jq
    ripgrep
    tmux
  ];

  # Puts fish in /etc/shells. Making it the login shell is one `chsh` on day
  # one (docs/studio.md): nix-darwin only sets shells for users it manages.
  programs.fish.enable = true;
  environment.shells = [ pkgs.fish ];

  # Remote Login. Keys only: the password stays for sudo and the keyboard.
  # If FileVault is turned on, its unlock over SSH runs before the data volume
  # holding this file is unlocked, so these lines should not affect it. That
  # is unverified; the FileVault test in docs/studio.md checks it.
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
  # anyone logs in. Joining is one manual `tailscale up` with a tagged auth key
  # (docs/studio.md); the key never goes in this repo.
  services.tailscale.enable = true;

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

  # nix-darwin owns Homebrew: a cask or formula not listed here is removed on
  # the next switch. Homebrew itself is installed by hand first (docs/studio.md).
  # Nothing is listed yet; claude-code arrives in phase 3.
  homebrew = {
    enable = true;
    onActivation = {
      autoUpdate = false;
      upgrade = false;
      cleanup = "uninstall";
    };
  };
}
