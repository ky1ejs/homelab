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
| Config | [`mac/`](../mac/): a nix-darwin flake with one host, `mac-studio` |
| Dotfiles | [kylejs-macOS-bootstrap](https://github.com/ky1ejs/kylejs-macOS-bootstrap), checked out at `~/kylejs-macOS-bootstrap` and linked with its `link.sh` |
| Day one | [`mac/bootstrap`](../mac/bootstrap), then the steps below |
| Apply | `~/homelab/mac/apply` on the Mac |
| Access | Tailscale node `mac-studio.tail3df177.ts.net`, tagged `tag:mac-studio`. SSH over the tailnet is Tailscale SSH; SSH on the LAN takes keys only. Screen Sharing logs in with the account password |
| LAN | Its own VLAN and firewall zone, *Mac Studio*: `192.168.40.2`, `mac-studio.lan`. See [The network](#the-network) |

`bin/homelab` stays the NAS's tool. The Mac has its own, [`mac/apply`](../mac/apply).

## What phase 1 sets, and what it leaves alone

[`mac/hosts/mac-studio.nix`](../mac/hosts/mac-studio.nix) sets:

- the host name `mac-studio`, the New York time zone, Nix with flakes, weekly
  store garbage collection;
- Remote Login on, **with password and keyboard-interactive login off**, so SSH
  takes keys only. The keys are [`mac/keys/kylejs.pub`](../mac/keys/kylejs.pub);
- `tailscaled` as a launchd daemon, so the Mac is on the tailnet before anyone
  logs in, restarted by launchd if it exits. With Tailscale SSH on (day one,
  step 8) it answers port 22 on the tailnet address itself, so the keys above
  guard LAN SSH only;
- never sleep, disks never sleep, restart after a power failure or a freeze,
  plus `pmset` settings nix-darwin has no option for (standby, Power Nap,
  wake on LAN);
- no automatic macOS updates, while security responses and XProtect data still
  install themselves;
- no guest account;
- Homebrew's packages: Ghostty, plus `xcodes` and `aria2` for installing
  Xcode. Anything installed with `brew` by hand and not listed is removed at
  the next apply;
- the Dock: Safari, Ghostty, Activity Monitor, System Settings and Xcode, with
  no recent apps. Each apply resets it to that list. Xcode's tile points at
  `/Applications/Xcode.app`, the link `mac/bootstrap` makes to the pinned
  version, so it shows a question mark until that step has run;
- the wallpaper, [`mac/wallpaper/mac-studio-dark.png`](../mac/wallpaper/),
  set by `desktoppr` from a launchd agent at each login. A wallpaper picked by
  hand lasts until the next login. The wallpaper's README says how to remake
  it from kylejs-site's design tokens.

Xcode is pinned in [`mac/xcode-version`](../mac/xcode-version), to the
version Cut's CI builds with. `mac/bootstrap` installs it with `xcodes`, with
the iOS simulator and the Metal toolchain, and links it at
`/Applications/Xcode.app`. The config cannot do this: `xcodes` signs in to
Apple's developer site with an Apple ID.

Nix keeps `trusted-users` at root alone. A trusted user can hand the daemon
options it runs as root, so trusting `kylejs` would make anything running as
`kylejs` root without sudo.

This repo holds no dotfiles. Fish, starship and git come from
kylejs-macOS-bootstrap, the same files the laptop uses: the Studio checks it
out and runs its `link.sh`, which links `Dotfiles/` and `bin/` into the home
directory and does nothing else. An edit on either Mac reaches the other with
a commit and a `git pull`, with no rebuild. What differs per machine goes in
`~/.config/git/config.local`, which git does not track.

That repo's `install.sh` is **not** run here: it installs GUI apps, writes
Dock and Finder plists, asks questions, and waits for an Xcode from the App
Store, where this host pins Xcode by version. nix-darwin installs the tools the
dotfiles expect (`starship`, `git-lfs`).

Left out, on purpose:

| Not set | Why |
|---|---|
| FileVault | On, turned on by hand: it needs a password, which does not belong in a config. See [FileVault](#filevault) |
| Auto-login | Off, because macOS turns it off when FileVault is on. The FileVault unlock logs `kylejs` in instead |
| An Apple Account | Signed in by hand, for Find My. nix-darwin cannot sign in, nor turn off the keychain sync that [The network](#the-network) needs off |
| Screen Sharing | macOS only turns it on in System Settings. It logs in with the account password, not a key |
| The screen lock | Since macOS 13 it needs the admin password or an MDM profile, so `mac/bootstrap` sets it |
| The `agent` user, CI runners, Claude Remote Control, Colima, models | Phases 2 to 5 |
| The login shell | nix-darwin only sets shells for users it creates. `chsh` once, below |
| Commit signing | The laptop signs with GPG; this host holds no signing key, so its `config.local` turns signing off |

## Before day one

1. **Public key.** [`mac/keys/kylejs.pub`](../mac/keys/kylejs.pub) holds the
   public half of the laptop's key for the Studio, made in 1Password. To add
   or replace one, print the keys 1Password's agent serves (plain `ssh-add -L`
   asks the macOS agent instead and finds none):

   ```sh
   SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock" ssh-add -L
   ```

   With no key in the file the config warns on every apply, and LAN SSH is
   closed because password login is off. Over the tailnet, Tailscale SSH
   checks the device's tailnet identity instead of a key, so the phone needs
   no SSH key at all and every key stays in 1Password.
2. **Tailscale policy.** The tailnet runs the default allow-all policy, so a
   tag on its own restricts nothing. First look at the admin console's
   Machines page for devices shared into or out of the tailnet: the rules
   below use `autogroup:member`, which leaves out users who reach a device
   through sharing. Add any such user to `src`.

   Then delete the `acls` block, and the `ssh` block if the file has one,
   and add every block below at the top level of the file: the
   `tag:mac-studio` entry in `tagOwners`, then `grants`, `postures`, `ssh`,
   `tests` and `sshTests`. Keep the existing `tagOwners` entries and the
   `nodeAttrs` that turn Funnel on. Without `postures` the save fails with
   *SSH rule referenced unknown posture "posture:apple"*:

   ```jsonc
   "tagOwners": {
     // keep the existing tag:vault-mcp and tag:gmail-mcp entries
     "tag:mac-studio": ["autogroup:admin"]
   },
   "grants": [
     // Your devices reach everything, as under allow-all. The Funnel tags and
     // tag:mac-studio get no grant: they start no connections on the tailnet.
     { "src": ["autogroup:member"], "dst": ["*"], "ip": ["*"] }
   ],
   "postures": {
     "posture:apple": { "assertions": ["node:os IN ['macos', 'ios']"] }
   },
   "ssh": [
     {
       "action": "check",
       "src": ["autogroup:member"],
       "srcPosture": ["posture:apple"],
       "dst": ["tag:mac-studio"],
       "users": ["kylejs"],
       "checkPeriod": "12h"
     }
   ],
   "tests": [
     // Your devices still reach each other, the NAS included, and the Studio.
     { "src": "<your login>", "accept": ["<your login>:22", "tag:mac-studio:22", "tag:mac-studio:5900"] },
     // The internet-facing connectors reach nothing on the Studio.
     { "src": "tag:gmail-mcp", "deny": ["tag:mac-studio:22", "tag:mac-studio:5900"] },
     { "src": "tag:vault-mcp", "deny": ["tag:mac-studio:22", "tag:mac-studio:5900"] },
     // The Studio reaches nothing.
     { "src": "tag:mac-studio", "deny": ["<your login>:22"] }
   ],
   "sshTests": [
     { "src": "<your login>", "srcPostureAttrs": { "node:os": "macos" }, "dst": ["tag:mac-studio"], "check": ["kylejs"], "deny": ["root"] },
     { "src": "<your login>", "srcPostureAttrs": { "node:os": "ios" }, "dst": ["tag:mac-studio"], "check": ["kylejs"] },
     // The NAS is yours and untagged, but runs Linux: no shell on the Studio.
     { "src": "<your login>", "srcPostureAttrs": { "node:os": "linux" }, "dst": ["tag:mac-studio"], "deny": ["kylejs"] }
   ]
   ```

   `<your login>` is the account the laptop's Tailscale is signed in to:
   `tailscale status --self --peers=false` on the laptop shows it in the
   second column. It need not be the login you use elsewhere, GitHub
   included. Tailscale runs
   `tests` and `sshTests` on every policy change and refuses a change that
   breaks one, so they stand in for checking by hand that the laptop still
   reaches the NAS. If either connector stops working after the save, its
   sidecar needed a tailnet destination after all: grant it that one
   destination, never `*`.

   The `ssh` rule lets your Macs and iPhone log in as `kylejs`, and nobody
   else, root included. The NAS is one of your devices too and runs agents all
   day, so `srcPosture` keeps Linux devices out. If the policy editor refuses
   `postures` on this plan, tag the NAS instead: tagged devices are not in
   `autogroup:member`.

   `check` asks you to sign in to Tailscale again in a browser at most every
   12 hours. It checks your account, not the device: once you have signed in,
   any of your Apple devices connects without asking until the 12 hours are
   up. Sign in as that same account, on this tailnet, and open the link the
   current `ssh` printed: Tailscale answers a link from an earlier try, or a
   sign-in as another account, with *This authentication link could not be
   located*. An SSH from a script on the laptop stalls on that browser
   sign-in, so scripts use the LAN address and the key instead. `accept`
   drops the prompt.

   Then generate an auth key: tagged `tag:mac-studio`, pre-approved, not
   ephemeral, single use. Tagged nodes have key expiry off, which avoids the
   lockout [`host.md`](host.md) describes for the NAS.
3. **On hand:** a display, keyboard and mouse for Setup Assistant; an Ethernet
   cable and a free switch port; the UPS.

## Day one

1. **Setup Assistant.** Account name **`kylejs`** (full name as you like). It
   becomes the only admin and is hard to rename later. Sign in to the Apple
   Account, then turn off its password sync for this Mac: System Settings →
   your name → iCloud → Passwords → *Sync this Mac* off. Synced Wi-Fi
   passwords would put the Studio back on Trusted ([The
   network](#the-network)). Turn FileVault on ([FileVault](#filevault)). Give
   the account a long password: Screen Sharing and the FileVault unlock both
   take it.
2. **Update macOS once,** to the latest, in System Settings. Automatic updates
   go off at the first apply.
3. **Screen Sharing on, for `kylejs` only:** System Settings → General →
   Sharing → Screen Sharing, then its info button: under *Allow access for*,
   choose *Only these users* and leave `kylejs`. Screen Sharing logs in with
   the account password, so it is the one password login reachable from the
   LAN and the tailnet. On the LAN, only the Trusted network reaches port
   5900 ([The network](#the-network)).
4. **Bootstrap,** in Terminal:

   ```sh
   bash -c "$(curl -fsSL https://raw.githubusercontent.com/ky1ejs/homelab/main/mac/bootstrap)"
   ```

   [`mac/bootstrap`](../mac/bootstrap) asks for the account password twice,
   then:

   - sets **the screen lock**: the password as soon as the screensaver starts
     or the display sleeps (after 10 minutes, set by the config). The config
     cannot set this: macOS ignores the old `askForPassword` setting and
     takes it only with the admin password or from MDM. To check it, start
     the screensaver from a hot corner, then press a key: it asks for the
     password at once;
   - installs **Homebrew**, which brings the Command Line Tools and so `git`;
   - installs **Nix** with the upstream installer. Not Determinate Systems'
     installer: it installs Determinate Nix, which manages Nix itself, and
     this config would then need `nix.enable = false`;
   - clones this repo to `~/homelab`, with an SSH push URL, and runs the
     first `mac/apply`, which installs Ghostty and `xcodes` through Homebrew.
     That run has no `darwin-rebuild` yet, so it uses the one
     `mac/flake.lock` pins;
   - installs **Xcode** at the version in `mac/xcode-version`, links it at
     `/Applications/Xcode.app`, and adds the iOS simulator and the Metal
     toolchain. `xcodes` asks for your Apple ID and its two-factor code, and
     the first run asks you to agree to Xcode's licence. The download is
     large, so this step takes the longest.

   It skips any part already done, so run it again after a failure. A
   `pmset ... was refused` warning means that setting does not exist on this
   Mac, not that the apply failed. If the first switch stops on *unexpected
   files in /etc* (the Nix installer's `/etc/nix/nix.conf`, `/etc/bashrc` or
   `/etc/zshrc`), move each file it names to `<file>.before-nix-darwin` with
   `sudo mv` and run `~/homelab/mac/apply` again. nix-darwin refuses to
   overwrite a file it did not write. Open a new terminal afterwards.
5. **Dotfiles:**

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
   SSH and need the laptop's key: commit and push from the laptop, or
   forward the agent for that one session with `ssh -A`. Do not set
   `ForwardAgent` for the Studio in the laptop's `~/.ssh/config`. The Studio
   will run CI jobs and agents, and while an agent is forwarded any process
   running as `kylejs`, and root, can use the laptop's keys, its GitHub key
   included.

   fish rewrites `Dotfiles/.config/fish/fish_variables`, which that repo
   tracks, whenever a universal variable changes, so the Studio's checkout
   shows it modified. A pull fails only when the laptop commits that file
   too: then run `git -C ~/kylejs-macOS-bootstrap checkout -- Dotfiles/.config/fish/fish_variables`
   and pull again. Untracking the file in that repo would end this, but the
   next pull on the laptop would delete the laptop's copy, so copy it aside
   first.
6. **Fish as the login shell:** `chsh -s /run/current-system/sw/bin/fish`.
7. **Git, per machine:** write `~/.config/git/config.local`:

   ```ini
   [user]
     email = …
   [commit]
     gpgsign = false
   ```

   Git refuses to commit without the email, on purpose, and without
   `gpgsign = false` every commit fails for want of a signing key.
8. **Join the tailnet** with the tagged key from above:

   ```sh
   sudo tailscale up --auth-key=<the key> --advertise-tags=tag:mac-studio --ssh
   ```

   `--ssh` turns on Tailscale SSH, and tailscaled keeps the setting across
   restarts. It needs Tailscale 1.104.4 or later: on older servers a
   session allowed to forward could reach root-only sockets (Tailscale
   bulletin TS-2026-014). The nixpkgs pin ships 1.104.5.

9. **Check it.** These use full tailnet names: tailscaled on macOS does not
   take over DNS, so a short name like `mac-studio` may resolve through the LAN
   and test the wrong path. From the laptop, in bash or zsh (`op=` is not
   fish syntax), with 1Password's agent:

   ```sh
   op="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
   ssh kylejs@mac-studio.tail3df177.ts.net true                          # Tailscale SSH, no key
   SSH_AUTH_SOCK="$op" ssh -A kylejs@mac-studio.tail3df177.ts.net ssh-add -L  # prints the laptop's key: a forwarded push works
   ssh -o "IdentityAgent=\"$op\"" -o PreferredAuthentications=publickey kylejs@<LAN address> true   # the key in mac/keys/kylejs.pub
   ```

   The first connection to each name asks whether to trust the host key:
   answer `yes`. The `true` lines print nothing when they work. The
   `IdentityAgent` value keeps its inner quotes because the socket path has
   a space in it, and `ssh -o` splits on spaces.

   The LAN address is `192.168.40.2`, or `mac-studio.lan` from a device that
   uses the UniFi gateway for DNS. The last line is the way in when Tailscale
   is down. On the Mac:

   ```sh
   sudo sshd -T | grep -Ei 'passwordauth|kbdinteractive'   # both "no"
   sysadminctl -guestAccount status                        # disabled
   pmset -g                                                # sleep 0, disksleep 0
   sudo systemsetup -getrestartpowerfailure                # On
   dscacheutil -q host -a name kyles-nas.tail3df177.ts.net # a 100.x address
   nc -z -G 3 -w 3 kyles-nas.tail3df177.ts.net 22 && echo "open: the tag policy is not applied" || echo "blocked, as intended"
   ```

   The `nc` line prints *blocked, as intended* when the policy works. macOS's
   `nc` needs `-G` to give up on a connection the policy drops; `-w` alone
   leaves it waiting. The line above it shows the name resolved, so a block
   is not a DNS failure.

   Then SSH in from the phone: any SSH app, with the Tailscale app
   connected, to `kylejs@mac-studio.tail3df177.ts.net`, and no key. If the app
   refuses to connect without a password or key, log in as `kylejs+password`
   and type anything: Tailscale SSH ignores it.
10. **Done when** a pulled power cable leaves the Mac back on the tailnet,
    reachable over SSH, after one [FileVault unlock](#filevault) and nothing
    else.

## FileVault

Decided 2026-11-13: on. macOS turns auto-login off when FileVault is on, and
phases 2 and 3 need a logged-in session. Unlocking over SSH gives one anyway:
it logs in the user who unlocks.

After a restart or a power cut the disk stays locked. Until it is unlocked,
only a pre-boot SSH server answers, on the LAN address alone: `tailscaled`
lives on the locked disk. It takes the account password, even though the
config turns password login off. That setting lives on the locked disk too.
Unlock from Trusted:

```sh
ssh kylejs@192.168.40.2
```

It asks for the password, prints *System successfully unlocked*, and closes.
Then the Studio rejoins the tailnet and `kylejs` is logged in:
`ssh ms 'stat -f %Su /dev/console'` prints `kylejs`. Name the address rather
than `ssh ms`: while the laptop's Tailscale is connected, `ms` means the
tailnet, which is still down.

Tested from the laptop on Trusted. From away from home, the route is SSH to
the NAS, then from there to `kylejs@192.168.40.2`, which the firewall allows
([The network](#the-network)). That route is untested. `ssh -J` through the
NAS may not work, because its `sshd` refuses `-L` and `-J` needs the same
forwarding.

## The network

Decided 2026-11-12. Day one put the Studio on the Trusted VLAN, beside the
laptop and phone. It moved to its own before phase 2 runs CI jobs and agents
there. Both the VLAN and the firewall live in UniFi, not in this repo, so this
section is their record. It was read back from UniFi Network 10.6.109's
Integration API.

| | |
|---|---|
| Network | *Mac Studio*, VLAN 5, `192.168.40.0/24`, mDNS forwarding off |
| Port | The Office Switch port the Studio is cabled to: native VLAN *Mac Studio*, tagged VLANs blocked |
| Address | Fixed IP `192.168.40.2`, Local DNS Record `mac-studio.lan`. Not `.local`, which macOS keeps for Bonjour. The name resolves only for devices that ask the UniFi gateway for DNS |
| Zone | *Mac Studio*, holding that network alone |

The zone's policies, ahead of UniFi's defaults, which block everything else
on the LAN:

| From | To | Allows |
|---|---|---|
| Trusted network | Studio | TCP 22 and 5900, and UDP 5900 to 5902 for High Performance Screen Sharing |
| The NAS, `192.168.3.2` | Studio | TCP 22, for the [FileVault](#filevault) unlock from away |
| Studio | Gateway | DNS (TCP and UDP 53) and DHCP (UDP 68 to 67), then a block on everything else, ping included |
| Studio | Internet | Everything: UniFi's default |

Replies come back on every Allow. UniFi refuses *allow return traffic* on a
policy to the gateway, but there the default *Gateway → Mac Studio: Allow
All* carries the replies. The Studio can start nothing towards the laptop,
the phone or the NAS.

The NAS moved the same day, into an *IoT* zone that holds the IoT network
alone. It was in the *Internal* zone with Trusted before. Trusted reaches it,
and it can start nothing towards Trusted. A Media network briefly shared the
zone. It had no Wi-Fi and no clients, so it was deleted.

**Two ways around the VLAN, both closed by hand:**

- **Wi-Fi.** A saved home network puts the Studio on that network's VLAN as
  well, beside the cable. Satti-private did, and gave it a Trusted address
  until it was forgotten. Wi-Fi stays on, with no home network saved.
- **The Apple Account's password sync** brings forgotten Wi-Fi passwords
  back, so it is off on the Studio (day one, step 1).

mDNS forwarding is off on the *Mac Studio* network, so Bonjour does not show
the Studio to Trusted. Connect Screen Sharing to `vnc://mac-studio.lan`, or to
the tailnet name, not to the Studio in Finder's sidebar. The sidebar entry
can also be a cached record of an address the Studio no longer has.

## Keeping it current

- `mac/apply --pull` pulls this checkout and the dotfiles checkout, then
  applies. `mac/apply build` builds without switching.
- A dotfile change is a commit to kylejs-macOS-bootstrap, never to this repo.
  Guard a tool not every Mac has with `type -q` in fish.
- A flake only sees files git tracks. `mac/apply` warns when `mac/` has
  uncommitted changes, because a new file nobody staged is silently absent.
- Xcode moves when Cut's CI does: change `mac/xcode-version` in a PR, then run
  `~/homelab/mac/bootstrap` again on the Studio, which pulls, installs the
  new version and moves the link. Remove the old one with `xcodes uninstall <version>`.
- Monthly, with the Xcode chore, update the lock on a branch, so
  `check-mac.yml` evaluates it before the Studio applies it. On the Studio,
  in an `ssh -A` session for the push:

  ```sh
  cd ~/homelab && git switch -c flake-update
  nix flake update --flake ./mac && mac/apply build
  git commit -am "mac: update flake.lock" && git push -u origin flake-update
  ```

  Open the PR on GitHub and merge it once CI is green. Then
  `git switch main && git branch -D flake-update && mac/apply --pull`, and
  update macOS by hand. Updating the lock on main and applying it skips CI,
  and the next `--pull` fails once main's lock moves.
- [`check-mac.yml`](ci.md) evaluates the config on every change to `mac/`, so a
  typo fails in CI rather than on the Mac. It cannot build the config: that
  needs a Mac.
