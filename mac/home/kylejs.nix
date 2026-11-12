# kylejs's shell on the Studio, through home-manager.
#
# Ported from kylejs-macOS-bootstrap rather than running its install.sh here.
# That repo is built for a laptop: GUI apps, Dock and Finder plists, prompts,
# and it symlinks the whole of ~/.config into its checkout, which would fight
# home-manager for the same files. Only the parts that matter over SSH are
# here: fish, starship and git. Language version managers (rbenv, pyenv, jenv,
# fnm) and the Spotify-specific lines are left out; nothing on this host
# needs them.
{ pkgs, ... }:

{
  home.stateVersion = "26.05";

  # home-manager's fish turns on man page caches, but there is no man package
  # on this host for them to index, which only produces a warning.
  programs.man.generateCaches = false;

  programs.fish = {
    enable = true;
    # Homebrew is installed by hand under /opt/homebrew, outside Nix's PATH.
    shellInit = ''
      if test -x /opt/homebrew/bin/brew
        /opt/homebrew/bin/brew shellenv | source
      end
    '';
    shellAliases = {
      gs = "git status";
      gd = "git diff";
      ga = "git add";
      gpl = "git pull";
      gps = "git push";
      gpsu = "git push -u origin HEAD";
      gcm = "git commit -m";
      gcam = "git commit -a -m";
      gco = "git checkout";
      gcob = "git checkout -b";
      gcl = "git clean -fd";
      gre = "git rebase";
      grec = "git rebase --continue";
      gres = "git rebase --skip";
      grea = "git rebase --abort";
      gfu = "git fetch upstream -p";
    };
  };

  programs.starship = {
    enable = true;
    settings = {
      gcloud.disabled = true;
      git_metrics.disabled = false;
    };
  };

  programs.git = {
    enable = true;
    lfs.enable = true;
    ignores = [ "*~" ".DS_Store" ];
    attributes = [ "*.plist diff=plist" ];
    settings = {
      # No email here: git refuses to commit until config.local gives one,
      # the same as on the laptop.
      user = {
        name = "Kyle Satti";
        useConfigOnly = true;
      };
      alias = {
        cam = "commit -am";
        cm = "commit -m";
      };
      diff.plist.textconv = "plutil -convert xml1 -o -";
      init.defaultBranch = "main";
      pull.rebase = true;
      push.default = "simple";
      # The laptop signs commits with GPG. This host holds no signing key.
      commit.gpgsign = false;
    };
    # ~/.config/git/config.local, written by hand: user.email at least.
    includes = [ { path = "config.local"; } ];
  };
}
