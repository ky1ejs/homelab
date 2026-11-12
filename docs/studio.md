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
| Config | [`mac/`](../mac/): a nix-darwin flake with one host, `studio` |
| Dotfiles | [kylejs-macOS-bootstrap](https://github.com/ky1ejs/kylejs-macOS-bootstrap), checked out at `~/kylejs-macOS-bootstrap` and linked with its `link.sh` |
| Apply | `~/homelab/mac/apply` on the Mac |
| Access | Tailscale node `studio`, tagged `tag:studio`. SSH over the tailnet is Tailscale SSH; SSH on the LAN takes keys only |

`bin/homelab` stays the NAS's tool. The Mac has its own, [`mac/apply`](../mac/apply).

## What phase 1 sets, and what it leaves alone

[`mac/hosts/studio.nix`](../mac/hosts/studio.nix) sets:

- the host name `studio`, the New York time zone, Nix with flakes, weekly
  store garbage collection;
- Remote Login on, **with password and keyboard-interactive login off**, so SSH
  takes keys only. The keys are [`mac/keys/kylejs.pub`](../mac/keys/kylejs.pub);
- `tailscaled` as a launchd daemon, so the Mac is on the tailnet before anyone
  logs in. With Tailscale SSH on (day one, step 10) it answers port 22 on the
  tailnet address itself, so the keys above guard LAN SSH only;
- never sleep, disks never sleep, restart after a power failure or a freeze,
  plus `pmset` settings nix-darwin has no option for (standby, Power Nap,
  wake on LAN);
- no automatic macOS updates, while security responses and XProtect data still
  install themselves;
- Homebrew under nix-darwin, with `cleanup = "uninstall"`: anything installed
  with `brew` by hand is removed on the next apply. List it in the config
  instead.

This repo holds no dotfiles. Fish, starship and git come from
kylejs-macOS-bootstrap, the same files the laptop uses: the Studio checks it
out and runs its `link.sh`, which links `Dotfiles/` and `bin/` into the home
directory and does nothing else. An edit on either Mac reaches the other with
a commit and a `git pull`, with no rebuild. What differs per machine goes in
`~/.config/git/config.local`, which git does not track.

That repo's `install.sh` is **not** run here: it installs GUI apps, writes
Dock and Finder plists, asks questions, and waits for `/Applications/Xcode.app`
where this host pins Xcode by version. nix-darwin installs the tools the
dotfiles expect (`starship`, `git-lfs`).

Left out, on purpose:

| Not set | Why |
|---|---|
| FileVault, auto-login | Undecided (see below), and both need a password, which does not belong in a config |
| An Apple Account | Undecided (see below) |
| Screen Sharing | macOS only turns it on in System Settings |
| The screen lock | Since macOS 13 it needs the admin password or an MDM profile, so it is a day-one command |
| The `agent` user, CI runners, Claude Remote Control, Colima, models | Phases 2 to 5 |
| The login shell | nix-darwin only sets shells for users it creates. `chsh` once, below |
| Commit signing | The laptop signs with GPG; this host holds no signing key, so its `config.local` turns signing off |

## Before day one

1. **Public key.** Add the public half of the laptop's 1Password SSH key
   (`ssh-add -L` prints it) to [`mac/keys/kylejs.pub`](../mac/keys/kylejs.pub)
   and commit it. Until then the config warns on every apply, and LAN SSH is
   closed because password login is off. Over the tailnet, Tailscale SSH
   checks the device's tailnet identity instead of a key, so the phone needs
   no SSH key at all and every key stays in 1Password.
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
   ],
   "ssh": [
     {
       "action": "check",
       "src": ["autogroup:member"],
       "dst": ["tag:studio"],
       "users": ["kylejs"],
       "checkPeriod": "12h"
     }
   ]
   ```

   This replaces the default `acls` block, so it is the tailnet's first
   restrictive rule. Check after saving that the laptop still reaches the NAS.

   The `ssh` rule lets your own devices log in as `kylejs` and nobody else,
   root included. `check` asks you to sign in to Tailscale again in a browser
   when a device has not done so in the last 12 hours, so a phone left
   unlocked is not a standing way in. `accept` drops that prompt.
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
3. **Screen Sharing on:** System Settings → General → Sharing. Then **the
   screen lock**: the password as soon as the screensaver starts or the
   display sleeps (after 10 minutes, set by the config).

   ```sh
   sudo sysadminctl -screenLock immediate -password -   # asks for the password
   ```

   To check it, start the screensaver from a hot corner, then press a key:
   it asks for the password at once. The config cannot set this: macOS ignores the old `askForPassword`
   setting and takes it only with the admin password or from MDM. The
   config does turn the guest account off.
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
7. **Dotfiles:**

   ```sh
   git clone https://github.com/ky1ejs/kylejs-macOS-bootstrap ~/kylejs-macOS-bootstrap
   git -C ~/kylejs-macOS-bootstrap remote set-url --push origin git@github.com:ky1ejs/kylejs-macOS-bootstrap.git
   ~/kylejs-macOS-bootstrap/link.sh
   ```

   `link.sh` moves anything already at a link's path aside to
   `<path>.backup.<timestamp>`, including a `~/.config` the installers made.
   Copy back anything in it you need.

   It lives outside `~/Developer` on purpose: the shared git config rewrites
   `https://github.com/` to SSH for repos under `~/Developer`, and the Studio
   holds no GitHub key, so pulls stay anonymous over HTTPS. Pushes go over
   SSH with the laptop's 1Password key, forwarded by `ssh -A kylejs@studio`
   (or `ForwardAgent yes` for `studio` in the laptop's `~/.ssh/config`).
8. **Fish as the login shell:** `chsh -s /run/current-system/sw/bin/fish`.
9. **Git, per machine:** write `~/.config/git/config.local`:

   ```ini
   [user]
     email = …
   [commit]
     gpgsign = false
   ```

   Git refuses to commit without the email, on purpose, and without
   `gpgsign = false` every commit fails for want of a signing key.
10. **Join the tailnet** with the tagged key from above:

    ```sh
    sudo tailscale up --auth-key=<the key> --advertise-tags=tag:studio --ssh
    ```

    `--ssh` turns on Tailscale SSH, and tailscaled keeps the setting across
    restarts. It needs Tailscale 1.104.4 or later: on older servers a
    session allowed to forward could reach root-only sockets (Tailscale
    bulletin TS-2026-014). The nixpkgs pin ships 1.104.5.

11. **Check it,** from the laptop and on the Mac:

    ```sh
    ssh kylejs@studio                                  # from the laptop, over the tailnet
    ssh -A kylejs@studio ssh-add -L                    # prints the laptop's key: pushes will work
    sudo sshd -T | grep -Ei 'passwordauth|kbdinteractive'   # both "no"
    sysadminctl -guestAccount status                   # disabled
    pmset -g                                           # sleep 0, disksleep 0
    systemsetup -getrestartpowerfailure                # On
    nc -zw3 kyles-nas 22 && echo "open: the tag policy is not applied"
    ```

    Then SSH in from the phone: any SSH app, with the Tailscale app
    connected, to `kylejs@studio`, and no key. If the app refuses to connect
    without a password or key, log in as `kylejs+password` and type anything:
    Tailscale SSH ignores it.
12. **Done when** a pulled power cable leaves the Mac back on the tailnet,
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

- `mac/apply --pull` pulls this checkout and the dotfiles checkout, then
  applies. `mac/apply build` builds without switching.
- A dotfile change is a commit to kylejs-macOS-bootstrap, never to this repo.
  Guard a tool not every Mac has with `type -q` in fish.
- A flake only sees files git tracks. `mac/apply` warns when `mac/` has
  uncommitted changes, because a new file nobody staged is silently absent.
- Monthly, with the Xcode chore: `nix flake update` in `mac/`, apply, then
  update macOS by hand.
- [`check-mac.yml`](ci.md) evaluates the config on every change to `mac/`, so a
  typo fails in CI rather than on the Mac. It cannot build the config: that
  needs a Mac.
