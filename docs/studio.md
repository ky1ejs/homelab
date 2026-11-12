# The Mac Studio

The homelab's second host: a Mac Studio (M5 Max, 64 GB, 1 TB) for Cut's iOS
CI, Claude Remote Control sessions that build and test iOS, and local models.
The plan, the reasoning and the rejected options live in the decision record
in the vault, *DR 2026-11-10 Mac build server for homelab*. This page is the
runbook for what is built.

When a step here turns out different on the host, correct the page against
the host, the same rule [`host.md`](host.md) follows.

| | |
|---|---|
| Hardware | Mac Studio, M5 Max (18-core CPU, 40-core GPU), 64 GB, 1 TB, Apple silicon |
| Admin user | `kylejs`, created by Setup Assistant |
| Config | [`mac/`](../mac/): a nix-darwin flake with one host, `studio`, plus home-manager for `kylejs` |
| Apply | `~/homelab/mac/apply` on the Mac |
| Access | Tailscale node `studio`, tagged `tag:studio`; SSH by key only |

`bin/homelab` stays the NAS's tool. The Mac has its own, [`mac/apply`](../mac/apply).

## What phase 1 sets, and what it leaves alone

[`mac/hosts/studio.nix`](../mac/hosts/studio.nix) sets:

- the host name `studio`, the New York time zone, Nix with flakes, weekly
  store garbage collection;
- Remote Login on, **with password and keyboard-interactive login off**, so SSH
  takes keys only. The keys are [`mac/keys/kylejs.pub`](../mac/keys/kylejs.pub);
- `tailscaled` as a launchd daemon, so the Mac is on the tailnet before anyone
  logs in;
- never sleep, disks never sleep, restart after a power failure or a freeze,
  plus `pmset` settings nix-darwin has no option for (standby, Power Nap,
  wake on LAN);
- no automatic macOS updates, while security responses and XProtect data still
  install themselves;
- Homebrew under nix-darwin, with `cleanup = "uninstall"`: anything installed
  with `brew` by hand is removed on the next apply. List it in the config
  instead.

[`mac/home/kylejs.nix`](../mac/home/kylejs.nix) gives `kylejs` fish, starship
and git, ported from kylejs-macOS-bootstrap. That repo's `install.sh` is **not**
run here: it is built for a laptop (GUI apps, Dock and Finder plists, prompts),
waits for `/Applications/Xcode.app` where this host pins Xcode by version, and
symlinks all of `~/.config` into its checkout, which home-manager also writes.

Left out, on purpose:

| Not set | Why |
|---|---|
| FileVault, auto-login | Undecided (see below), and both need a password, which does not belong in a config |
| An Apple Account | Undecided (see below) |
| Screen Sharing | macOS only turns it on in System Settings |
| The `agent` user, CI runners, Claude Remote Control, Colima, models | Phases 2 to 5 |
| The login shell | nix-darwin only sets shells for users it creates. `chsh` once, below |
| Commit signing | The laptop signs with GPG; this host holds no signing key |

## Before day one

1. **Public keys.** Add the 1Password SSH key (laptop) and a Termius key
   (phone) to [`mac/keys/kylejs.pub`](../mac/keys/kylejs.pub), one per line,
   and commit them. Until then the config warns on every apply, and SSH is
   closed because password login is off.
2. **Tailscale policy.** The tailnet runs the default allow-all policy, so a
   tag on its own restricts nothing. Add the tag, then grants that keep every
   current device and tag as it is and give `tag:studio` nothing outbound:

   ```jsonc
   "tagOwners": {
     // keep the existing tag:vault-mcp and tag:gmail-mcp entries
     "tag:studio": ["autogroup:admin"]
   },
   "grants": [
     { "src": ["autogroup:member"], "dst": ["*"], "ip": ["*"] },
     { "src": ["tag:vault-mcp", "tag:gmail-mcp"], "dst": ["*"], "ip": ["*"] }
   ]
   ```

   This replaces the default `acls` block, so it is the tailnet's first
   restrictive rule. Check after saving that the laptop still reaches the NAS.
   Then generate an auth key: tagged `tag:studio`, pre-approved, not
   ephemeral, single use. Tagged nodes have key expiry off, which avoids the
   lockout [`host.md`](host.md) describes for the NAS.
3. **On hand:** a display, keyboard and mouse for Setup Assistant; an Ethernet
   cable and a free switch port; the UPS.

## Day one

1. **Setup Assistant.** Account name **`kylejs`** (full name as you like). It
   becomes the only admin and is hard to rename later. Skip the Apple Account
   unless that decision is made. Choose *not now* for FileVault until its test
   below.
2. **Update macOS once,** to the latest, in System Settings. Automatic updates
   go off at the first apply.
3. **Screen Sharing on:** System Settings → General → Sharing.
4. **Homebrew**, which also installs the Command Line Tools and so `git`:

   ```sh
   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
   ```

5. **Nix**, with the upstream installer:

   ```sh
   sh <(curl --proto '=https' --tlsv1.2 -L https://nixos.org/nix/install)
   ```

   Not Determinate Systems' installer: it installs Determinate Nix, which
   manages Nix itself, and this config would then need `nix.enable = false`.
   Open a new terminal afterwards.
6. **Clone and apply:**

   ```sh
   git clone https://github.com/ky1ejs/homelab ~/homelab
   ~/homelab/mac/apply
   ```

   The first run has no `darwin-rebuild` yet, so the script runs the one
   `mac/flake.lock` pins, then asks for your password for `sudo`. A
   `pmset ... was refused` warning means that setting does not exist on this
   Mac, not that the apply failed.

   If the first switch stops on *unexpected files in /etc* (the Nix
   installer's `/etc/nix/nix.conf`, `/etc/bashrc` or `/etc/zshrc`), move each
   file it names to `<file>.before-nix-darwin` with `sudo mv` and run
   `mac/apply` again. nix-darwin refuses to overwrite a file it did not write.
7. **Fish as the login shell:** `chsh -s /run/current-system/sw/bin/fish`.
8. **Git email:** write `~/.config/git/config.local` with `[user] email = …`.
   Git refuses to commit without it, on purpose.
9. **Join the tailnet** with the tagged key from above:

   ```sh
   sudo tailscale up --auth-key=<the key> --advertise-tags=tag:studio
   ```

10. **Check it,** from the laptop and on the Mac:

    ```sh
    ssh kylejs@studio                                  # from the laptop, over the tailnet
    sudo sshd -T | grep -Ei 'passwordauth|kbdinteractive'   # both "no"
    pmset -g                                           # sleep 0, disksleep 0
    systemsetup -getrestartpowerfailure                # On
    nc -zw3 kyles-nas 22 && echo "open: the tag policy is not applied"
    ```

11. **Done when** a pulled power cable leaves the Mac back on the tailnet,
    reachable over SSH, without anyone touching it.

## Open decisions

Phase 1 works either way on each of these; the config does not assume an
answer.

- **FileVault.** macOS turns auto-login off when FileVault is on, and phases 2
  and 3 need a logged-in session. macOS Tahoe can unlock FileVault over SSH
  before login, over Ethernet. That probably happens before `tailscaled` starts,
  because the disk holding it is still locked, so unlocking from away from home
  would mean SSH to the NAS, then SSH from there to the Studio's LAN address.
  (`ssh -J` through the NAS may not work: its `sshd` refuses `-L`, and `-J`
  needs the same forwarding.) Test it before relying on it: turn it on,
  reboot, unlock from another machine on the LAN, then see whether the
  auto-login session starts.
- **An Apple Account on the Studio.** Signing in gives Find My (and Activation
  Lock) and lets `xcodes` download Xcode. Without it, download the Xcode
  `.xip` on the laptop and install it with `xcodes install --path`.
- **A VLAN for the Studio.** The router is UniFi, so it can. If FileVault is
  on, the VLAN has to let the NAS reach the Studio on port 22, or the unlock
  above has no route.

## Keeping it current

- `mac/apply --pull` pulls this checkout and applies it. `mac/apply build`
  builds without switching.
- A flake only sees files git tracks. `mac/apply` warns when `mac/` has
  uncommitted changes, because a new file nobody staged is silently absent.
- Monthly, with the Xcode chore: `nix flake update` in `mac/`, apply, then
  update macOS by hand.
- [`check-mac.yml`](ci.md) evaluates the config on every change to `mac/`, so a
  typo fails in CI rather than on the Mac. It cannot build the config: that
  needs a Mac.
