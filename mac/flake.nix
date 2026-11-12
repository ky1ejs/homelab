{
  # The Mac Studio, as code. One host, `studio`, applied on the Mac with
  # `mac/apply` (or `sudo darwin-rebuild switch --flake ~/homelab/mac#studio`).
  # docs/studio.md is the runbook: day one, what this config does not do, and
  # the decisions still open.
  #
  # The base host is phase 1 of the setup plan in the decision record. CI
  # runners, Claude Remote Control, Colima and the model server are phases 2
  # to 5.
  description = "nix-darwin config for the homelab Mac Studio";

  # Inputs are fetched by plain git and from the NixOS channel server, not by
  # `github:` references. `github:` goes through api.github.com, which rate-limits
  # anonymous callers, and CI fetches this flake on every run.
  inputs = {
    nixpkgs.url = "https://channels.nixos.org/nixpkgs-26.05-darwin/nixexprs.tar.xz";

    nix-darwin = {
      url = "git+https://github.com/nix-darwin/nix-darwin?ref=nix-darwin-26.05&shallow=1";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  # Dotfiles (fish, starship, git) are not here. They live in
  # kylejs-macOS-bootstrap, which the Studio checks out and links with its
  # link.sh, the same files the laptop links (docs/studio.md, day one).
  outputs = { self, nixpkgs, nix-darwin }: {
    # The locked darwin-rebuild, for the first switch, before one is installed:
    # `sudo nix run ~/homelab/mac#darwin-rebuild -- switch --flake ~/homelab/mac#studio`.
    # mac/apply does this when it finds no darwin-rebuild.
    packages.aarch64-darwin.darwin-rebuild = nix-darwin.packages.aarch64-darwin.darwin-rebuild;

    darwinConfigurations.studio = nix-darwin.lib.darwinSystem {
      modules = [
        ./hosts/studio.nix
        {
          # nix-darwin records the flake's revision, so `darwin-version` says
          # which commit is applied. A dirty tree reports no revision.
          system.configurationRevision = self.rev or self.dirtyRev or null;
        }
      ];
    };
  };
}
