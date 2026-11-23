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
| Apply | `hl apply` on the Mac, which pulls `~/homelab` and the dotfiles, then runs `~/homelab/mac/apply`. See [The homelab command](#the-homelab-command) |
| Access | Tailscale node `mac-studio.tail3df177.ts.net`, tagged `tag:mac-studio`. SSH over the tailnet is Tailscale SSH; SSH on the LAN takes keys only. Remote Management (Screen Sharing or Apple Remote Desktop) logs in with the account password |
| Unlock | `ssh ms-unlock` after any restart but `fdesetup authrestart`, then log `worker` in with Screen Sharing. See [FileVault](#filevault) |
| Remote Claude | `worker`'s Remote Control server for Cut, the `cut` environment. See [Claude Remote Control](#claude-remote-control) |
| Shipping apps | `app-store`, which signs any app and uploads it to TestFlight with an App Store Connect key in worker's home. See [Building and shipping apps](#building-and-shipping-apps) |
| iOS CI | Two Tart VM runners for Cut, as `worker`. See [iOS CI](#ios-ci) |
| Simulators | simslim slims every simulator `worker` boots, to about 2.3 GB from about 13; `sim-full NAME` makes one full. See [Claude Remote Control](#claude-remote-control) |
| Docker | Colima, one Linux VM as `worker`, for the sessions' Compose services. See [Linux containers](#linux-containers) |
| Usage | `mac-usage report`: CI minutes, agent hours, host load and power, and what they would have cost hosted. See [Usage and cost](#usage-and-cost) |
| Monitoring | The NAS's Beszel hub reads its agent: alerts when it is off or its disk fills, and a weekly email. See [Monitoring](#monitoring) |
| LAN | Its own VLAN and firewall zone, *Mac Studio*: `192.168.40.2`, `mac-studio.lan`. See [The network](#the-network) |

## The homelab command

`homelab`, or `hl`, is the mac studio's management command, the
counterpart of the NAS's [`bin/homelab`](../bin/homelab). It puts the
commands below behind one name, and adds `status` and `logs`. Every
command it wraps still works by its own name, and the rest of this page
still shows them that way. [`mac/cli`](../mac/cli/) installs it on `PATH`
for both users, so `ssh -t ms hl status` works from the laptop or the
phone.

```sh
ssh -t ms hl status
```

| Command | Runs | As |
|---|---|---|
| `hl status` | The host (uptime, console user, load, memory pressure, swap, free disk), the config (applied commit against the checkout and `origin/main` as of the last fetch), each of worker's launchd agents, the CI slots, both heartbeats' last result, and the root daemons | either |
| `hl logs [name] [-f] [-n <lines>]` | `tail` of worker's logs and the root daemons'. No name lists them. A name matches worker's `<name>.log`, `claude-rc-<name>.log` or `ci-<name>.log`, so `hl logs cut` and `hl logs runner-1` work. The root daemons' `/var/log` logs are `daemon-<name>`, such as `hl logs daemon-mac-usage`, because `mac-usage` has a log in each place | either |
| `hl usage [args]` | `mac-usage report [args]`, so `hl usage --since 2026-11-17` | either |
| `hl apply [args]` | `~/homelab/mac/apply --pull [args]`, which pulls both checkouts, then applies. `--no-pull` applies them as they stand. The rest go to `mac/apply`: `build`, `--no-wait`, `--wait-minutes <n>`. `--pull` is still taken | kylejs |
| `hl update` | `git pull --ff-only` in `~/homelab` and the dotfiles checkout, with no apply | kylejs |
| `hl git <args>` | `git` in `~/homelab` | kylejs |
| `hl shell` | `worker-shell` | kylejs |
| `hl relogin` | `relogin` | kylejs |
| `hl image build [--source <img>]` | `build-image` | worker |
| `hl drain wait <slot> [minutes]` | `ci-drain wait dev.kylejs.ci-runner-<slot> <minutes> hl`, 45 minutes by default, the apply's wait, under its own holder name, `hl`. The slot takes no new job for up to the minutes plus 5. It returns once the slot's current job, if any, has ended, and says when the hold ends; `hl drain release` ends it sooner. If the job outlasts the wait, it releases the slot and exits 1 | either |
| `hl drain release <slot>` | `ci-drain release` for the slot's `hl` hold and an apply's, as a cut-off apply leaves. Never `build-image`'s, which it renews every minute while it uses the slot | either |

Which user runs what:

- **kylejs**, over SSH. Worker's agents, logs and CI state sit in worker's
  home, which only worker and root can read, so `status` and `logs` read
  them as worker through `sudo -u worker`, and only `launchctl print` runs
  as root. Use `ssh -t` so `sudo` can ask for the password, once per run. Without a terminal, `status` shows the rest and says what it
  left out. `drain` runs `ci-drain` as worker through `sudo`, as an apply
  does.
- **worker**, in `hl shell`. worker has no `sudo`, so it runs only its own
  parts. `image build` must run here, because the VMs need worker's login
  session. The commands that touch kylejs's checkouts, or need `sudo`,
  refuse to run as worker and name the right user.

`hl status` exits 1 when any row says FAIL, so a script can check it. It
reads and changes nothing: it does not fetch, so `origin/main` is as of the
last fetch, which it dates, and it takes no git lock. A CI slot counts only
while its runner agent runs, and a phase written before the last restart
shows as such.

It does not wrap `session-cleanup`. That is the agent sessions' own command,
run from a session's folder, and their permission rule names it exactly
([Fresh main, and any repo](#fresh-main-and-any-repo)), so a second name for
it would route around that rule. It does not restart the Mac or unlock it
either: see [FileVault](#filevault).

`mac/cli/homelab` is the script. `mac/cli/default.nix` fills in the user
names, the paths, the CI slots and the drain minutes from the rest of the
config, so a slot added in `mac/ci` shows in `hl status` and `hl drain` with
no change to the script. The disk and swap thresholds, the heartbeats'
interval and the daemon list are copied by hand, and each source says so. CI runs it against stand-ins for macOS's commands
(`mac/cli/test-homelab`), which checks its parsing and user checks, not
that macOS still prints what the stand-ins copy.

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
- a launchd daemon, [`mac/lock-screen`](../mac/lock-screen), that locks the
  screen when a Screen Sharing or Apple Remote Desktop session ends and when
  someone is logged in at the console. See [The screen lock](#the-screen-lock);
- command-line tools from Nix, pinned in `mac/flake.lock`, `xcodes`
  included: nixpkgs wraps it with `aria2`, which speeds up the Xcode
  download;
- GUI apps from Homebrew: Ghostty, as a cask in `/Applications`. Nix would
  put an app in a separate `/Applications/Nix Apps` folder, and nix-darwin's
  check of that folder then stops every apply over SSH. Anything installed
  with `brew` by hand and not listed is removed at the next apply;
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
| Auto-login | Off, because macOS turns it off when FileVault is on. After a restart, `worker` logs in with Screen Sharing |
| An Apple Account | Signed in by hand, for Find My. nix-darwin cannot sign in, nor turn off the keychain sync that [The network](#the-network) needs off |
| Remote Management | macOS only turns it on in System Settings, and runs it or Screen Sharing, not both. It logs in with the account password, not a key |
| The screen lock's delay | Since macOS 13 it needs the admin password or an MDM profile, so `mac/bootstrap` sets it. The config adds the daemon that relies on it |
| Creating the `worker` user | FileVault takes only a user created with an admin password. By hand, in [Claude Remote Control](#claude-remote-control) |
| Local models | Phase 5 |
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
   the account a long password: Remote Management and the FileVault unlock
   both take it.
2. **Update macOS once,** to the latest, in System Settings. Automatic updates
   go off at the first apply.
3. **Remote Management on, for `kylejs` only:** System Settings → General →
   Sharing → Remote Management, then its info button: under *Allow access
   for*, choose *Only these users*, leave `kylejs`, and give it *Observe* and
   *Control*. It replaces Screen Sharing: macOS runs one or the other. The
   Screen Sharing app and Apple Remote Desktop both connect to it, on port
   5900. It logs in with the account password, so it is the one password
   login reachable from the LAN and the tailnet. On the LAN, only the Trusted
   network reaches port 5900 ([The network](#the-network)). ARD's reporting
   port, 3283, is open in neither the zone nor the tailnet grant, so ARD's
   status and reports may not work; observing and controlling don't need it.
   Switched from Screen Sharing on 2026-11-13.
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
     password at once. [The screen lock](#the-screen-lock)'s daemon relies
     on it;
   - installs **Homebrew**, which brings the Command Line Tools and so `git`;
   - installs **Nix** with the upstream installer. Not Determinate Systems'
     installer: it installs Determinate Nix, which manages Nix itself, and
     this config would then need `nix.enable = false`;
   - clones this repo to `~/homelab`, with an SSH push URL, and runs the
     first `mac/apply`, which installs Ghostty through Homebrew and `xcodes`
     through Nix.
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
phases 2 and 3 need a logged-in session: `worker`'s, which runs the [Claude
Remote Control](#claude-remote-control) servers. Unlocking over SSH does not
give one. It finishes the boot at the login window with nobody logged in, so
after it `worker` logs in [with Screen Sharing](#logging-worker-in). Found
2026-11-13, on two restarts: the console user was `root`, the login window.

Running the servers as launchd daemons instead, which need no login, was
tried and ruled out the same day. A simulator boots without a login, but
Claude's sign-in lives in worker's login keychain, and the long-lived token
from `claude setup-token` does not work with Remote Control
([claude-code#96076](https://github.com/anthropics/claude-code/issues/96076)).

After a restart or a power cut the disk stays locked. Until it is unlocked,
only a pre-boot SSH server answers, on the LAN address alone: `tailscaled`
lives on the locked disk. It takes the account password, even though the
config turns password login off. That setting lives on the locked disk too.

### Restarting

A planned restart skips the unlock when started this way:

```sh
ssh -t ms sudo fdesetup authrestart
```

`-t` lets `sudo` ask for the password. `fdesetup` may then ask for a FileVault
user and password as well: give `worker`'s. It skips the unlock; whether it
also logs `worker` in, saving the Screen Sharing step, is not yet tested on the
Studio. After it, `ssh ms 'stat -f %Su /dev/console'` prints `worker` if it
did and `root` if it stopped at the login window.

Any other restart stops at the unlock, power cuts included. To test the
unlock, restart with `ssh -t ms sudo shutdown -r now`.

### Unlocking

From the laptop, at home or away:

```sh
ssh ms-unlock
```

It asks for `worker`'s password, prints *System successfully unlocked*, and
closes. A minute or two later the Studio is back on the tailnet, at the login
window: `ssh ms 'stat -f %Su /dev/console'` prints `root`. Then [log `worker`
in](#logging-worker-in).

`ms-unlock` lives in kylejs-macOS-bootstrap's SSH config, beside `ms`. It
always means `worker@192.168.40.2` and offers no key, because the pre-boot
server takes only the password. `ssh ms` cannot unlock: while the laptop's
Tailscale is connected, `ms` means the tailnet name, and the tailnet is down
until the unlock. On a Mac without the dotfiles, run
`ssh worker@192.168.40.2`. `kylejs`'s password unlocks too, with `kylejs`
logged in and no servers.

Away from home, the laptop's Tailscale must be connected. It sends traffic for
`192.168.40.2` to the NAS, which advertises that one address as a subnet
route. The NAS forwards the connection from its own address, `192.168.3.2`,
which UniFi's NAS rule lets in ([The network](#the-network)). SSH is
encrypted end to end, so the NAS never sees the password. At home with
Tailscale connected, the unlock takes the same path through the NAS, and so
does anything else sent to `192.168.40.2`. macOS picks the route by address,
so `ms-unlock` cannot skip the NAS while Tailscale is connected. `ssh ms`
never takes the NAS: it uses the tailnet name while the laptop's Tailscale is
connected and the LAN address straight from Trusted while it is not. Point
anything else at the tailnet name too, Screen Sharing included, and only the
unlock goes through the NAS. If the NAS is down, unlock from home with
Tailscale disconnected: the laptop then reaches the Studio straight from
Trusted.

Tested 2026-11-13, from Trusted and from a phone hotspot.

### Logging worker in

After the unlock, connect with Screen Sharing or Apple Remote Desktop as
`kylejs`. The Studio's login window shows: pick `worker` and type worker's
password. When the window closes, [`mac/lock-screen`](../mac/lock-screen)
locks the screen with `worker` still logged in, and within a minute the
servers show in the Claude app. Until then nothing of worker's runs, the
heartbeat included, so Healthchecks.io emails about the silence.

macOS has no supported way to log a desktop user in from a shell, so this
step can't be scripted while FileVault is on.

### What the unlock from away needs

Set once, on 2026-11-13. Check these when `ssh ms-unlock` hangs from away.

1. **The NAS advertises the route.** On the NAS:

   ```sh
   $(getcfg SHARE_DEF defVolMP -f /etc/config/def_share.info)/.qpkg/Tailscale/tailscale \
     set --advertise-routes=192.168.40.2/32
   ```

   `set` changes that one setting. `tailscale up` with the same flag would
   reset the NAS's other settings.
2. **The route is approved.** In the Tailscale admin console, Machines, the
   NAS shows a *Subnets* badge and lists `192.168.40.2/32` as approved.
3. **The policy lets you reach it.** The member grant to `*` in
   [Before day one](#before-day-one), step 2, covers subnet routes. If that
   grant is ever narrowed, keep TCP 22 to `192.168.40.2/32` for
   `autogroup:member`.
4. **UniFi lets the NAS in.** The NAS to Studio policy on TCP 22, in
   [The network](#the-network).

On the laptop, `route -n get 192.168.40.2` names a `utun` interface while
Tailscale is connected and the route is live.

## The screen lock

macOS locks the screen when the display sleeps, 10 minutes after the last
input, for a user whose screen lock is *immediate* (`mac/bootstrap` sets
kylejs's). It does not lock when a Screen Sharing session ends: Screen Sharing
drives the console session itself, so the Studio's screen comes back on at an
unlocked desktop. In a High Performance session the Studio's displays go dark
while connected and turn back on, unlocked, at the disconnect. After `ssh
ms-unlock` the Studio is at the login window, which needs no lock.

Remote Management serves Screen Sharing and Apple Remote Desktop on the same
port, 5900, so all of this applies to an ARD session too. Neither has a
setting to lock at the end of a session. So
[`mac/lock-screen`](../mac/lock-screen), a launchd daemon running as root,
puts the display to sleep, which locks it:

- when the last connection to port 5900 closes;
- when a user other than the login window is at the console and nobody is
  connected. That covers a login at the keyboard, which then asks for the
  password a second time;
- when the daemon starts with a user at the console and nobody connected. An
  apply that changes the script restarts it, so that apply locks the screen.

It locks only a user whose screen lock is immediate. Each user at the console
needs `sysadminctl -screenLock immediate -password -`, run once as that user.
Its log is `/var/log/lock-screen.log`, one line per lock.

To test it, with nobody at the Studio's keyboard:

1. Connect with Screen Sharing and log in, then close the window. Within a few
   seconds the Studio's own screen sleeps; a key press shows the lock screen.
   Try it in both standard and High Performance mode.
2. While connected in High Performance mode, run
   `ssh ms "netstat -anp tcp | grep '[.]5900 '"`. It should show an
   `ESTABLISHED` line. If it shows none, the daemon cannot see High
   Performance sessions, and test 1 fails in that mode.
3. Restart and `ssh ms-unlock`. Once the Studio is back on the tailnet, its
   screen should show the login window, not a desktop.
4. `ssh ms cat /var/log/lock-screen.log` shows a line for each Screen Sharing
   lock above.

All four passed on 2026-11-13, so `pmset displaysleepnow` from a root daemon
locks the screen the way the display's own sleep does. The lock after an ARD
session works too.

ARD's *Curtain* hides the Studio's screen during a session, but it can't be
applied at the lock screen. It works only once the session is unlocked, so the
Studio's screen shows the desktop for a moment first. Once `worker` exists it
may not be needed:

5. With `worker` at the Studio's lock screen, connect as `kylejs` with Screen
   Sharing, then again in High Performance mode. kylejs's session should run
   in the background: the Studio's monitor stays on worker's lock screen, and
   its keyboard cannot reach kylejs's desktop. If so, Curtain isn't needed.

## Claude Remote Control

Phase 3, built before phase 2. Claude sessions started from the Claude app or
claude.ai run on the Studio as `worker`, a standard user with no admin rights.
[`mac/claude`](../mac/claude/) sets what worker runs: a
`claude remote-control` server for each repo in its `repos` list, one more for
any other repo, a job that keeps the listed repos current, a heartbeat and two
commands for reaching worker's session over SSH. Cut is the only listed repo.

| Environment, as the Claude app lists it (the repo's name, or the folder's without a remote) | Folder | Each session from the app gets | At once, standing session included |
|---|---|---|---|
| `cut` | `~worker/code/cut` | its own git worktree | 11, the standing session's process once it runs among them |
| `any` | `~worker/code/any` | an empty folder, to clone into | 6, likewise |

Claude Code 2.1.322 serves one folder per server, which is why there are two
kinds. [Fresh main, and any repo](#fresh-main-and-any-repo) says what each
does and when to use which.

Each server is a launchd agent in worker's login session, started at login
and restarted within 30 seconds whenever it exits. It runs only while worker
is logged in, which after a restart means the [unlock](#unlocking) and then
[worker's login](#logging-worker-in).

**Restarts keep the environment.** Each server keeps one standing session in
its own folder, titled `mac-studio cut main checkout` and
`mac-studio any repo folder`. While it has one, Claude Code records the
environment, keeps it when the server stops, and registers the same
environment when it starts again, so a star on `cut` or `any` in the app
survives a restart. Without one (`--no-create-session-in-dir`, used until
2026-11-15), every stop archived the server's sessions and deleted the
environment, and every start made a new one.

**Sessions come back, but maybe not their folders.** A restart no longer
archives sessions, and a session answers again after it. But when the server
stops, it first removes the worktree of each running session that has no
changes and no new commits, and the any-repo server's hook deletes each
running session's folder whose clones are clean and pushed. Ignored files
go with them: `.env`, `node_modules`, DerivedData. A session whose folder went
is back in the app without it, so start a new one for further work. A folder
with changes is kept, as before.

**After a long outage.** The record lasts four hours after a stop. A server
that starts later, such as after a power cut that waits at the unlock
overnight, makes a new environment, and its star has to be set again. The
old one is only marked offline, never deleted, and its sessions are not
archived: the app shows it beside the new one, offline. Archive its sessions
in the app. Claude Code has a message for an environment the service
cleaned up while its machine was offline, so the service may remove the
environment itself in time; nothing documents when.

The standing session works in the main checkout, or in `~/code/any`, not in a
worktree. Don't work in it: start a session from the app. worker's
`~/.claude/CLAUDE.md`, from
[`mac/claude/worker-CLAUDE.md`](../mac/claude/worker-CLAUDE.md), and
`~/code/any/AGENTS.md` tell the standing sessions to change nothing there.
A tracked change or a branch switch left in the main checkout would stop the
[repo sync](#fresh-main-and-any-repo) fast-forwarding it; untracked and
ignored files would not, but every new worktree's setup would not see them
either.

**There is no Bash sandbox.** It blocks CoreSimulator, which Cut's builds
and tests need. The boundary is worker itself: a standard user, with no SSH or
code-signing keys, and GitHub tokens that never reach this repo or
kylejs-macOS-bootstrap. It does hold an App Store Connect API key, which
signs through Apple's cloud and can do much in the App Store Connect account
([The key](#the-key)).

**worker's GitHub tokens.** A fine-grained token has one owner and the same
permissions on every repo it covers, so worker has up to three, each a file
in `~/.config/github-tokens`:

| File | Reaches | Can |
|---|---|---|
| `cut-watch` | every `cut-watch` repo, including new ones | read, write, create repos |
| `ky1ejs-read`, optional | every `ky1ejs` repo, including new ones | read only |
| `ky1ejs`, optional | only the `ky1ejs` repos picked in its settings | read and write |

[`mac/claude/github-token`](../mac/claude/github-token) hands git and `gh`
the token for the repo. For a `ky1ejs` repo it asks GitHub whether the
`ky1ejs` token can see it, and otherwise uses `ky1ejs-read`. No token with
write access may ever cover this repo or kylejs-macOS-bootstrap: `hl apply` (`mac/apply --pull`) runs this repo's main as root and
pulls kylejs-macOS-bootstrap, and the NAS deploys this repo's main too, so
push access to either would let a session take over the Studio and the NAS.
That is why worker can't create `ky1ejs` repos: creating one needs
Administration on *All repositories*, which takes in both, and Administration
can remove branch protection, so protecting them would not help. Create
personal repos from an agent off the Studio, then add them to the `ky1ejs`
token ([A personal repo on the Studio](#a-personal-repo-on-the-studio)).

`ky1ejs-read` can't push, merge, run a workflow or change a setting, so it
can't reach the Studio or the NAS. What it adds is your private repos: a
session can read them, and so can anyone it is tricked into sending them to.
This repo and kylejs-macOS-bootstrap are public, so it adds nothing there.
Keep secrets out of private repos too, as this repo does.

Administration on the `cut-watch` token also lets a session delete a
`cut-watch` repo or change its settings and branch protection. All three are
plain text in worker's home, so any session can read them. Nobody at the
Studio's keyboard gets past worker's lock screen. Registering a GitHub App
needs a browser, so no token can do it.

**Build tools.** The config puts `xcbeautify` and `xcodegen` on PATH, from
Nix, for Cut's `ios/scripts`. `build-app.sh` stops without `xcbeautify`, and
`bootstrap.sh` otherwise installs `xcodegen` into `/usr/local`, which worker
cannot write. Cut pins `xcodegen` exactly (`XCODEGEN_REQUIRED_VERSION` in its
`ios/scripts/bootstrap.sh`, 2.44.1 on 2026-11-13), so recheck that
`mac/flake.lock`'s nixpkgs still has that version at the monthly flake
update.

**Dead code scanner.** The config puts `periphery` on PATH
([`mac/periphery`](../mac/periphery/)) for Cut's
`ios/scripts/periphery-scan.sh`, which stops without it. Cut uses
`agoodkind/periphery`, a community fork of the archived upstream, and pins
one release by version and sha256 in its `.github/workflows/ios-build.yaml`.
`mac/periphery/release.nix` copies both values, and the package is that
release's signed binary, unchanged. The CI image gets the same release from
the same file ([iOS CI](#ios-ci)).

**Simulator tool.** The config puts [idb](https://fbidb.io) on PATH: `idb`,
Meta's client, and `idb_companion`, the server it starts for a simulator.
Sessions use it to tap, type and read a simulator's screen without opening
Simulator on the Mac's display ([`mac/idb`](../mac/idb/)). Both come from one
idb release, pinned in `mac/idb/release.nix`, and `idb` uses that release's
companion unless `IDB_COMPANION_PATH` says otherwise. nixpkgs' own
`idb-companion` is 1.1.8, from 2022, older than the Xcode 26 the current one
needs; and the client's generated code needs a newer protobuf than 26.05 has,
so it is built with nixpkgs-unstable's Python. A session installed both for
itself on 2026-11-13; [Tools installed outside the Nix
config](#tools-installed-outside-the-nix-config) has how to remove that copy.
`mac/claude/any-AGENTS.md` tells any-repo sessions that idb is there; Cut's
own `ios/AGENTS.md` does not yet, so a session on Cut's server finds it only
on PATH.

The client keeps its state and each companion's socket in a fixed
`/tmp/idb`, made by whoever runs idb first after a restart. Run idb on the
Mac only as worker (`worker-shell`). If sessions get permission errors under
`/tmp/idb`, another user made it; remove it with `sudo rm -rf /tmp/idb`.

**Video tool.** The config puts `ffmpeg` and `ffprobe` on PATH
([`mac/claude`](../mac/claude/)), so sessions can read screen recordings: a
model reads images, not video, so a session turns a recording from
`xcrun simctl io recordVideo`, or one from a device, into still frames.
worker's `~/.claude/CLAUDE.md` ("Screen recordings") gives sessions the
commands, and `mac/claude/any-AGENTS.md` points any-repo sessions to it. It
is nixpkgs' `ffmpeg-headless`: ffmpeg and ffprobe without ffplay, which needs
a display. Its closure is about 225 MB unpacked, 4 MB less than plain
`ffmpeg`'s, and its own decoders read the H.264 and HEVC those recordings use.
To pull one frame from a recording by hand, in `worker-shell`:

```sh
ffmpeg -nostdin -y -v error -ss 2.5 -i rec.mov -frames:v 1 frame.png
```

**Slim simulators.** [simslim](https://github.com/MobAI-App/simslim) slims
every simulator worker boots, iOS, tvOS, watchOS and visionOS
([`mac/simslim`](../mac/simslim/)). It
writes launchd disable overrides into the simulator's own store, so the host
is untouched. On 2026-11-20, on macOS 27.2.1 and Xcode 27.1, a stock iOS 27
simulator ran 291 processes and 12.97 GB by phys_footprint; slimmed, 75 and
2.34 GB. Upstream issue #73 says a slim simulator takes 620 MB on a macOS
27.0 host, so a fix there would cut it further.

A launchd agent in worker's session, `dev.kylejs.simslim-watch`, runs
`simslim watch`. It slims a shut-down simulator before it boots, and every
simulator again as it boots, Xcode's parallel-testing clones included. It
writes one JSON event per line to `~worker/Library/Logs/simslim-watch.jsonl`
and its errors to `simslim-watch.log` beside it. The heartbeat reports the
agent when it is not running. kylejs's simulators stay stock.

A slim simulator lacks, among others, push registration and StoreKit
(simslim category `store`), Sign in with Apple and iCloud (`icloud`),
universal links (`web`), Spotlight (`search`), widgets and Live Activities
(`widgets`), Siri (`siri`), HealthKit (`health`) and DeviceCheck
(`telemetry`). Contacts and Photos stay on, for Cut's friend finder and
image picker. `sim-full NAME [CATEGORY...]` turns those services back on in
one simulator until it shuts down: it boots it if needed, waits for the
agent's `slimmed` event for that boot, then runs `simslim enable`. For a
simulator that was already booted, it waits up to two of the agent's scans
for a new event before it takes the last one as this boot's.
`mac/claude/worker-CLAUDE.md` tells sessions when to use it, and the
sessions' autoMode allows it on their own simulators and blocks touching the
agent, a session's own `simslim watch` included. An apply that changes the
agent's plist restarts it, and the new agent slims every booted simulator
again, full ones included.

`simslim measure <udid>` and `simslim top` read memory with `top`, which
works only as the system's setuid `/usr/bin/top`. A Nix copy of `top` cannot
sample other processes and rejects `-stats mem`. nixpkgs' Claude Code
wrapper puts one first on every session's PATH, so both commands
failed with a bare *exit status 1* (2026-11-22). `mac/claude` now builds the
wrapper without that `top`, and `mac/simslim` wraps simslim with the
system's tool directories first on its PATH.

**Git LFS.** Cut keeps its images in LFS. Without the LFS filter a worktree
checks out pointer files, and the asset catalog fails with *AppIcon did not
have any applicable content*. `git lfs install` would write the filter into
`~/.gitconfig`, and Nix's git reads its system config from its own store
path, not `/etc/gitconfig`. worker has no dotfiles, so the servers and
`worker-shell` get the filter from git's `GIT_CONFIG_COUNT` and
`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` variables, set in their launchd
agents. These add to `~/.gitconfig`, so the token helper stays. A checkout
made before the filter was set still holds pointer files: run `git lfs pull`
once in it, from `worker-shell`.

**Commit author.** The same variables set `user.name` and `user.email` to
Kyle's, so commits from the sessions link to his GitHub account. Without them
git takes worker's full name and `worker@<hostname>`, and GitHub shows the
author as a bare "worker". The agent's `Co-Authored-By` trailer still marks
its commits. To check, have a new session from the Claude app run
`git var GIT_AUTHOR_IDENT`.

One claude.ai sign-in, worker's, serves the server. An API key or a
`claude setup-token` token does not work: Remote Control needs a full
claude.ai login.

### Setting it up

Once, in this order. Steps 1 and 2 run as `kylejs`; the rest in
`worker-shell`, a shell inside worker's login session.

1. **Create `worker`**, a standard user that can unlock FileVault:

   ```sh
   ssh -t ms
   sudo sysadminctl -addUser worker -fullName worker -password - -adminUser kylejs -adminPassword -
   sysadminctl -secureTokenStatus worker   # ENABLED
   sudo fdesetup list                     # lists worker
   ```

   `-password -` asks for worker's password; `-adminPassword -` for
   kylejs's. Keep worker's in 1Password: it is the unlock password from now
   on. `-fullName` is the name macOS shows, at the login window and in
   password prompts.

   If `fdesetup list` leaves worker out (it did on 2026-11-13), give worker
   a secure token if the status says DISABLED, then add it to FileVault. The
   add asks for an existing FileVault user, kylejs, then worker's password:

   ```sh
   sudo sysadminctl -secureTokenOn worker -password - -adminUser kylejs -adminPassword -
   sudo fdesetup add -usertoadd worker
   ```

   To change the shown name later:
   `sudo dscl . -change /Users/worker RealName OLD NEW`, then
   `sudo diskutil apfs updatePreboot /` so the FileVault unlock screen
   shows it too.
2. **Apply**, which installs Claude Code and writes worker's server:
   `~/homelab/mac/apply`. worker has no session yet, so nothing starts.
3. **Restart, unlock and log worker in:** `ssh -t ms sudo shutdown -r now`,
   then `ssh ms-unlock` with worker's password, then [log worker
   in](#logging-worker-in) with Screen Sharing. `ssh ms 'stat -f %Su
   /dev/console'` prints `worker`. The server starts and exits: no repo, no
   sign-in yet. worker's first login opens macOS's new-user Setup Assistant;
   finish it once, skipping the Apple Account.
4. **Lock worker's screen**, in `ssh -t ms worker-shell`:

   ```sh
   sysadminctl -screenLock immediate -password -
   ```

   It asks for worker's password, then asks for it as soon as the screensaver
   starts or the display sleeps, as kylejs's does. worker is the console user
   holding the GitHub and claude.ai sign-ins, and
   [`mac/lock-screen`](#the-screen-lock), which locks the screen after
   Screen Sharing, locks only a user set to immediate.
5. **Give worker its GitHub token and clone Cut**, in
   `ssh -t ms worker-shell`. First make a fine-grained token on GitHub with
   *Resource owner* `cut-watch`, *All repositories*, an expiry of a year,
   and these repository permissions: Administration, Contents, Issues, Pull
   requests and Workflows read and write; Actions and Commit statuses read.
   *All repositories* takes in repos created later; Administration is what
   lets it create them. As a `cut-watch` owner, kylejs's request is approved
   straight away. Then:

   ```sh
   mkdir -p -m 700 ~/.config/github-tokens
   bash -c 'umask 077; read -rsp "cut-watch token: " t; echo; printf "%s\n" "$t" > ~/.config/github-tokens/cut-watch'
   git config --global credential.https://github.com.helper ''
   git config --global --add credential.https://github.com.helper /run/current-system/sw/bin/github-token
   git config --global credential.https://github.com.useHttpPath true
   launchctl kickstart -k "gui/$(id -u)/dev.kylejs.claude-sync"
   ```

   The last line runs the [repo sync](#fresh-main-and-any-repo) now, which
   clones Cut to `~/code/cut`; otherwise it does so within 5 minutes.

   The token is a file, not a `gh auth login`, so the server's sessions read
   it without the keychain. The first `git config` line drops macOS's
   keychain helper for GitHub. GitHub emails before the token expires; renew
   it by writing the new one over the file.
6. **Sign in to claude.ai**, in the same shell: `claude auth login`. Open
   the link it prints on any device and paste the code back.
7. **Trust each server's folder and accept Remote Control**, in
   `worker-shell`. The first asks `Trust <folder>?` and
   `Enable Remote Control?`, the others only `Trust <folder>?`; answer `y`
   each time, wait for the session URL, then Ctrl-C. These runs keep
   `--no-create-session-in-dir`, so they leave no environment behind and the
   server starts its own standing session, with its own title:

   ```sh
   cd ~/code/cut && claude remote-control --spawn worktree --no-create-session-in-dir
   launchctl kickstart -k "gui/$(id -u)/dev.kylejs.claude-rc.cut"
   cd ~/code/any && claude remote-control --spawn worktree --no-create-session-in-dir
   launchctl kickstart -k "gui/$(id -u)/dev.kylejs.claude-rc.any"
   ```

   Each `kickstart` restarts a server, which had exited untrusted. Trusting
   `~/code/any` covers the session folders under it: Claude Code looks for
   trust in a folder's parents, up to the root of the git repo it is in, or
   up to `/` outside one.
8. **Point the heartbeat at Healthchecks.io.** Make two checks, each with a
   5-minute period and 10 minutes' grace: one for Remote Control (*Mac
   Studio Claude Remote*) and one for the host (*Mac Studio host*). In
   `worker-shell`, paste each one's ping URL at its prompt:

   ```sh
   bash -c 'umask 077; mkdir -p ~/.config/claude-rc; read -rp "Remote Control ping URL: " u; printf "%s\n" "$u" > ~/.config/claude-rc/ping-url'
   ```

   ```sh
   bash -c 'umask 077; read -rp "host ping URL: " u; printf "%s\n" "$u" > ~/.config/claude-rc/host-ping-url'
   ```

   Then run the heartbeat once:

   ```sh
   launchctl kickstart -k "gui/$(id -u)/dev.kylejs.claude-heartbeat"
   ```

   ```sh
   sleep 5; tail -n 4 ~/Library/Logs/claude-rc-heartbeat.log
   ```

   The log has a `remote control:` line and a `host:` line, each ending in
   `ok` or listing what is wrong, and both checks show up on Healthchecks.io.
   Without `host-ping-url`, both groups ping the first check (see [The
   heartbeat](#the-heartbeat)). The URLs stay on the mac studio, outside the
   repo.
9. **Done when** the Claude app lists the `cut` and `any` environments, a
   session on each answers, the screensaver from a hot corner asks for worker's password,
   and after a restart, `ssh ms-unlock` and worker's Screen Sharing login it
   comes back with nothing else run.

### A personal repo on the Studio

worker can't create `ky1ejs` repos (see [worker's GitHub
tokens](#claude-remote-control)). Create one from the laptop or a cloud
session. worker can read it straight away with `ky1ejs-read`; to work in it,
add it to the `ky1ejs` token.

1. Once, for reading: make a fine-grained token with *Resource owner*
   `ky1ejs`, *All repositories*, an expiry of a year, and Contents, Issues
   and Pull requests read only. Write it to
   `~/.config/github-tokens/ky1ejs-read` with the `bash -c` line from step
   5, `cut-watch` changed to `ky1ejs-read` in both places.
2. Once, for writing: make a fine-grained token with *Resource owner* `ky1ejs`, *Only
   select repositories*, an expiry of a year, and Contents, Issues, Pull
   requests and Workflows read and write; Actions and Commit statuses read.
   No Administration. Never pick `homelab` or `kylejs-macOS-bootstrap`. In
   `worker-shell`, write it to `~/.config/github-tokens/ky1ejs` with the
   `bash -c` line from step 5, `cut-watch` changed to `ky1ejs` in both
   places.
3. For each repo worker should change: GitHub, Settings, Developer settings, Fine-grained
   tokens, the token, *Edit*; add the repo under *Repository access*, then
   *Update*. The token's value stays the same, so nothing changes on the
   Studio.
4. Its sessions can use the `any` environment now. To give it a server of
   its own, add it to `repos` in
   [`mac/claude/default.nix`](../mac/claude/default.nix), apply, then trust
   `~worker/code/<name>` as in step 7 of [Setting it up](#setting-it-up). The
   repo sync clones it within 5 minutes.

### Fresh main, and any repo

**Fresh main.** A session started from the app on a repo's own server works
in a git worktree
that Claude Code branches from `origin/main`, never from the main checkout's
own files. Claude Code fetches before making one only when the repo has gone
24 hours without a fetch, so `origin/main` could be a day old. The repo sync,
[`mac/claude/sync-repos`](../mac/claude/sync-repos), runs every 5 minutes as
worker: it clones each repo in `repos` that is missing, fetches it, and
fast-forwards the main checkout when that is on main with no changes, so it
also looks current in `worker-shell`. A failed clone or fetch is tried again
30 seconds later, since right after a restart worker's session can start
before the network is up, and the heartbeat reports a second failure.

**Any repo.** The `any` environment runs in `~worker/code/any`, which is not a
git repo. Claude Code calls a `WorktreeCreate` hook, if one is set, instead of
`git worktree add`, so [`mac/claude/any-session`](../mac/claude/any-session),
set as that hook in `~/code/any/.claude/settings.json`, gives each session an
empty folder in `~/code/any/sessions`. `~/code/any/AGENTS.md`, from
[`mac/claude/any-AGENTS.md`](../mac/claude/any-AGENTS.md), tells the session
to clone the repo it is asked about there and push a branch before it ends.
Each session clones afresh, so it starts from the latest main.

When a session is archived, Claude Code calls the `WorktreeRemove` hook. The
script deletes the folder only if everything in it is a git clone with no
changes, untracked files, stashes or commits that no remote has; otherwise it
keeps the folder, and the server log says
`worktree removal failed, kept: <path>`. Claude Code keeps a repo server's
worktree for the same reasons, under `~/code/<name>/.claude/worktrees`.

**Kept folders are swept.** Every hour, and at each login,
[`mac/claude/sweep-sessions`](../mac/claude/sweep-sessions) looks at both
kinds. Most kept folders hold nothing a remote does not: Claude Code keeps a
subagent's worktree (`agent-<hex>`, from `isolation: worktree`) when the
subagent made commits, even pushed ones, until its own sweep at
`cleanupPeriodDays` (30 by default). On 2026-11-22 a day of Cut sessions left
60 worktrees, 263 GB. So a folder with no changes, no untracked files git
does not ignore and no commits that no remote-tracking branch has (for an
any-repo folder, on any local branch or the stash) is deleted once no process
works in it, nothing in it has changed and no transcript has recorded working
there for an hour (a subagent's `agent-*` worktree) or 4 days (any other,
which an open session may come back to: an open session writes no transcript
while it waits for a prompt). Ignored files are not counted, as the salvage
below does not save them. A session Claude Code lists as running in
`~worker/.claude/sessions` keeps its folder, and a worktree Claude Code has
locked for a process still running is left alone. Before a checkout goes, its
`scripts/teardown`, if it has one, runs for up to 2 minutes: Cut's drops the
worktree's databases and Redis keys. Nothing in such a folder is lost, so
nothing goes to salvage. A branch squash-merged
and deleted on GitHub reads as commits no remote has, so its worktree waits
for the 14-day rule below.

A folder with nothing in it changed for 2 hours has its DerivedData
deleted: folders named `.derivedData` or `DerivedData`, at most three levels
down, that git ignores. Xcode rebuilds them, so a session that comes back to
the folder builds from scratch once. They are most of a kept folder's size:
on 2026-11-15, 174 GB of the 180 GB in 18 Cut worktrees, which build into
`ios/.derivedData`. A command a session is running there, such as a build or
a shell, stops the trim, and each DerivedData folder is checked again just
before it goes. A server left running in the background does not: it has no
parent, and it does not use the build output. The trim keeps folder times as
they were, so it does not restart the next clock. A folder that no process of
worker's is working in, with nothing changed for 14 days, has its unsaved
work saved to
`~worker/code/salvage/<date>-<server>-<session>/`, then it is deleted, with a
worktree's branch. A session cut off by a restart, which never calls the
remove hook, is swept the same way. Each checkout's salvage is a few files:
`<checkout>.txt` (path, origin, branch, HEAD), a git bundle of the commits no
remote has, a patch of its changes, and a tarball of its untracked files.
Ignored files, such as build output and `node_modules`, are not saved.
Anything in an any-repo folder that is not a clone goes whole into
`other.tar.gz`. Salvage over 30 days old is deleted. If a folder's work
cannot all be saved, the folder stays and the heartbeat reports it.

**Periphery's cache is capped.** Periphery keeps a DerivedData folder per
project path and Xcode version in
`~worker/Library/Caches/com.github.peripheryapp`, so each worktree that runs
it adds one and none goes with its worktree: 95 GB on 2026-11-22. Each sweep
keeps the newest, and any changed in the last 10 minutes. Going from newest
to oldest, any other unchanged for 24 hours goes, and once those kept so far
fill 20 GB, every older one goes too. Nothing goes while a `periphery`
process runs.

A server left running in a kept folder keeps the folder from ever being
swept. On 2026-11-15 a Cut backend server (`bun run src/index.ts`) had held a
15 GB worktree for two days, and on 2026-11-17 a bun server and an Xcode
`DTServiceHub` had held one for three. A session that starts a server in the
background leaves it with no parent, so launchd (pid 1) holds it, even while
the session is still open. `session-cleanup` stops these when a session
ends, but not when a restart cut the session off.

Each sweep looks at the processes like that which have run for over a day.
When no command runs in their folder and no session has worked there for 4
days, the sweep stops each and what it started: SIGTERM, then SIGKILL for
what is left 10 seconds later. It first checks that each is still the
process it found. A session's own process does not work in its folder, so
the sweep reads where sessions worked from their transcripts: every record in
`~worker/.claude/projects/*/*.jsonl` names its working directory (`cwd`),
whichever folder the transcript is kept in. A session open in the app writes
nothing while it waits for a prompt, so 4 days covers a weekend; one idle
that long that still wants its server can start it again. A folder no
transcript names is never stopped in, since then the sweep cannot tell. The
heartbeat reports each stop once, with a success ping, and the rest, still
running, each with the command that stops it. One that does not stop is
reported both ways: as a sweep failure and as left running. That the
transcripts write `cwd` as compact JSON (`"cwd":"/Users/..."`) is untested
on the mac studio; if they do not, the sweep stops nothing and only reports,
as before.

The sweep also deletes simulators that test runs left behind when they were
killed before their own cleanup: those shut down with nothing in their folder
changed for 6 hours, among xcodebuild's clones for parallel testing
(`~worker/Library/Developer/XCTestDevices`, 12 GB on 2026-11-15) and the
`Cut Tests <pid>-<n>` devices Cut's `ios/scripts/test-modules.sh` makes.
A session's `<name> @<ID>` devices go by the same rule, and at once,
booted or not, when its session folder is gone.
Xcode's own devices stay, because Cut's test script copies its device from
them and CoreSimulator does not make them again. So the sweep does not run
`simctl delete unavailable`, which also takes devices whose runtime is only
unmounted for now, as after a restart; that is a step of the Xcode move in
[Keeping it current](#keeping-it-current). Each run ends by recording the
size of worker's biggest folders for the heartbeat's low-disk report.

To get work back, in `worker-shell`, from a clone of the same repo:

```sh
s=~/code/salvage/<date>-<server>-<session>
git fetch "$s/<checkout>.bundle" 'refs/*:refs/salvage/*'   # its commits, as refs/salvage/...
git apply "$s/<checkout>.patch"                             # its uncommitted changes
tar -xzf "$s/<checkout>-untracked.tar.gz"                   # its untracked files
```

A session in a clone gets the clone's instruction files (`AGENTS.md`,
`CLAUDE.md`), rules and skills once it reads a file there. It does not get
the clone's project settings: its hooks, permission rules, plugins and `env`,
nor its `.mcp.json` servers. Claude Code reads those only from the folder a
session starts in. Use a repo's own server for work that needs them, and add
a repo to `repos` when it is used often enough to be worth one.

**Sessions don't clash on files.** Each has its own worktree or clone. What
they share is the Mac: CPU, memory, and anything outside the folder, such as
iOS simulators, which Cut's test script gives one per checkout.

**Sessions clean up only what they started.** Every session runs as worker
in auto mode, so worker's permissions cannot tell one session's servers and
simulators from another's, and neither can auto mode's classifier, which
sees commands but not their output. On 2026-11-16 it refused a Cut session's
`xcrun simctl delete $U`, with the UDID read from a file in `/tmp`, and
`pkill -f "bun run src/index.ts"`, which matches every session's backend, so
the session asked Kyle to run them. Three things in
[`mac/claude`](../mac/claude) let a session stop its own and nothing else:

- [`session-cleanup`](../mac/claude/session-cleanup), on worker's `PATH`,
  works out the session's folder from where it runs, then stops every process
  working in that folder and deletes every simulator whose name ends in
  ` @<ID>`: ` @cut/X` for the worktree `~/code/cut/.claude/worktrees/X`,
  ` @any/X` for `~/code/any/sessions/X`. With them go the `Cut Tests` devices
  of the test runs it stopped. It leaves alone the session itself, its tools,
  such as MCP servers, and the rest of the command it runs in. Its tools are
  its children in its own process group. Claude Code runs each command in a
  shell with a process group of its own, so what a command started is
  stopped, even a server that `bash -c` exec'd and so made the session's
  child. Until 2026-11-18 any child that was not a shell counted as a tool,
  which spared those servers, and they outlived their sessions as the bun and
  `DTServiceHub` leftovers.
  `session-cleanup --dry-run` lists what it would stop. Run anywhere else,
  such as in a main checkout, it does nothing.
- worker's `~/.claude/CLAUDE.md` tells sessions to name their simulators that
  way, start servers from their folder, run `session-cleanup` when done, and
  never stop things by a pattern such as `pkill -f`.
- Both servers pass every session a settings file from the Nix store with
  `--settings`. It allows plain `session-cleanup` and
  `session-cleanup --dry-run` without the classifier, runs
  `session-cleanup --hook` as a `SessionEnd` hook when a session ends (not on
  `/clear`), and tells the classifier that the Mac is shared, that a
  session's own ` @<ID>` simulators are disposable, and that `--hook` and
  cleaning up from another session's folder are not for sessions. The hook
  runs `/run/current-system/sw/bin/session-cleanup`, so a change to the
  script alone does not restart the servers; a change to the settings does.

The sweep above deletes the simulators of a session cut off before its hook
ran. It stops the servers such a session leaves only once no session has
worked in their folder for 4 days; until then the heartbeat reports them.

### When the sign-in lapses

The heartbeat pings `/fail` with *sign-in or trust error* in the body. Run:

```sh
ssh -t ms relogin
```

It opens `claude auth login` inside worker's session, where worker's keychain
is unlocked, and attaches to it. Open the printed link on the phone or
laptop, sign in, paste the code back. It then restarts the server. Over
Tailscale SSH from the phone it works the same, with no key.

`relogin` and `worker-shell` run as `kylejs` and ask for kylejs's password
for `sudo`. Both use tmux inside worker's session: detach with Ctrl-b d to
leave a shell running, and the next `worker-shell` returns to it. A plain
`sudo -u worker` shell runs outside worker's session, where a sign-in cannot
reach worker's keychain.

tmux has mouse mode on, set by
[`mac/claude/worker-session`](../mac/claude/worker-session) each time either
one starts. Scroll with the trackpad or wheel to read back; press q or scroll
to the bottom to type again. In a full-screen program such as `less`, `man`
or `top`, the wheel sends it arrow keys instead, as it did before. Drag to
select: on release the text goes to the laptop's clipboard, and dragging
past the top or bottom edge scrolls. For more than a few screens, write the
output to a file in `/tmp`, which both users can read (`<command> >
/tmp/out.txt`), and copy it from the laptop with
`ssh ms cat /tmp/out.txt | pbcopy`. Hold Shift while dragging for Ghostty's
own selection, which sees only the visible screen.

### The heartbeat

Every 5 minutes, as worker, [`mac/claude/heartbeat`](../mac/claude/heartbeat)
runs two groups of checks, and pings a Healthchecks.io check for each.

Remote Control's checks say whether a session can start: each server's
launchd job is running, no server log has a sign-in or trust error since the
last run, and the repo sync ran in the last 30 minutes and failed for no
repo. They ping `~/.config/claude-rc/ping-url`.

The host's checks say whether the mac studio is healthy for the sessions and
CI it runs, and ping `~/.config/claude-rc/host-ping-url`. Without that file,
both groups ping the Remote Control check, and the body's first line says
which failed: *Remote Control servers are up. The mac studio itself has
problems:* when only the host's did. Before 2026-11-17 there was only the one
check, so a memory warning emailed as *Claude Remote* down.

The host's checks: the session sweep ran
in the last 6 hours and kept no folder it should have swept. It kills any xcodebuild of
worker's older than 2 hours and reports swap over 8 GB, memory pressure that
is critical or at warning for two runs in a row, a mac-usage sampler that has
written nothing for 10 minutes, and under 150 GB of
free disk, naming worker's biggest folders from the last sweep. That leaves
room before the CI runners stop at 50 GB ([iOS CI](#ios-ci)). Low disk pings
`/fail` only when it first falls under 150 GB, and again under 75 GB; while
it stays low it goes with the success ping, so a later failure still emails.
It reports when
Colima's Docker does not answer, and when a Postgres, Redis, S3 or other
database server runs on the host instead of in Docker
([Linux containers](#linux-containers)), and when worker has installed a
tool for itself outside the Nix config
([Tools installed outside the Nix config](#tools-installed-outside-the-nix-config)),
and when the last sweep found a server running with no parent in a kept
folder for over a day ([Fresh main, and any repo](#fresh-main-and-any-repo)), or
stopped one whose session had ended. Like
low disk, tools and leftover processes ping `/fail` only when a new one
appears, and otherwise go with the success ping. A stopped process goes with
the success ping once.
Each group pings its check when all is well, and `/fail` with its problems
otherwise. Healthchecks.io emails on `/fail`, and on silence: the mac studio
off, still locked, or at the login window with worker not logged in.

If `xcrun metal` finds no Metal toolchain, it runs
`xcodebuild -downloadComponent MetalToolchain`, which mounts it again, and
reports a problem only if that fails or `metal` is still missing. On
2026-11-13 the toolchain was still on disk from `mac/bootstrap` but not
mounted, after a restart at 13:45 that day; that the restart caused it is
inferred from timestamps. The first download that day failed with *Failed
fetching catalog* and a retry worked, so a failure is retried 5 minutes
later.

It never runs `claude auth status`, and nothing else that touches the
sign-in. It keeps each log to its newest 2,000 lines once it passes 10 MB.

Logs, as `kylejs`:

```sh
sudo tail -n 50 ~worker/Library/Logs/claude-rc-cut.log        # one server
sudo tail -n 20 ~worker/Library/Logs/claude-rc-heartbeat.log  # "remote control:" and "host:", each "ok" or the problems
sudo tail -n 20 ~worker/Library/Logs/claude-rc-sync.log       # clones and failures only
sudo tail -n 20 ~worker/Library/Logs/claude-rc-sweep.log      # folders swept, trimmed or kept, simulators deleted
```

### Still to test

Before 2026-11-21, the end of the return window:

- a server runs under launchd with no terminal. If its log shows it waiting
  for one, wrap the command in `/usr/bin/script -q /dev/null`;
- `claude auth login` prints a link and takes a pasted code, rather than
  only opening a browser on the Studio's screen;
- the heartbeat's error patterns match what a lapsed sign-in really prints;
- `fdesetup authrestart` with worker's password logs worker in;
- `sysadminctl -screenLock` takes worker's own password without admin rights;
- worker's token creates a private repo in `cut-watch`, with
  `gh repo create cut-watch/token-test --private`, then deletes it with
  `gh repo delete cut-watch/token-test --yes`;
- with both `ky1ejs` tokens, a picked repo pushes, a private repo that isn't
  picked clones but won't push, and a `git push --dry-run` from a clone of
  this repo fails with a 403;
- a Screen Sharing session as kylejs keeps the Studio's monitor on worker's
  lock screen ([The screen lock](#the-screen-lock), test 5);
- a Cut session pushes a branch over HTTPS and runs a simulator test;
- a new `cut` session's `git log -1 origin/main` matches GitHub's
  main, which shows the worktree branches from `origin/main` after the sync;
- an `any` session started from the app starts in `~/code/any/sessions/<name>`,
  reads `~/code/any/AGENTS.md`, and clones the repo it is asked about. This
  shows the server runs the `WorktreeCreate` hook from its folder's project
  settings;
- archiving that session deletes its folder when the clone is pushed and
  clean, and keeps it otherwise.
- `launchctl kickstart -k "gui/$(id -u)/dev.kylejs.claude-rc.cut"` in
  `worker-shell` keeps the `cut` environment: the app shows one `cut`, with
  its star. A `cut` session that only read a file, so its worktree is clean,
  answers after the restart, and its `pwd` and `git status` show whether
  its worktree is still there; the same with an `any` session whose clone
  is pushed. Record the result under [Sessions come back, but maybe not their
  folders](#claude-remote-control);
- `sudo lsof -a -u worker -d cwd` while a session runs a command lists its
  folder, which is how the sweep tells a folder in use. On 2026-11-15 it
  listed no `claude` process in any folder, so an open session that is not
  running a command does not show;
- in a `cut` session, `session-cleanup --dry-run` lists a backend it started
  in the background and a simulator it named ` @cut/<folder>`, and not the
  standing session's or another session's; archiving the session stops the
  backend and deletes the simulator (the `SessionEnd` hook), as `ps` and
  `xcrun simctl list devices` show. Time the hook with a booted simulator:
  it has 120 seconds;
- in a `cut` session with an MCP server, `ps -x -o pid,ppid,pgid,comm` shows
  the server in the session's process group and the session's command
  shells each in a group of their own. `session-cleanup --dry-run` lists a
  server the session started as a background task, including one started
  with `exec`, and does not list the MCP server. If the MCP server has a
  group of its own, `session-cleanup` would stop it: record that here and
  fix the script;
- the sweep, on a kept folder backdated with `touch -t`, saves its work and
  deletes it, and the salvage restores into a fresh clone;
- the sweep trims a kept Cut worktree untouched for 2 hours: its
  `ios/.derivedData` goes, the folder's other files and times stay, and its
  log line gives the size freed. A session left open but idle for 2 hours
  loses its DerivedData too, and its next build starts from scratch;
- the sweep deletes a pushed `agent-*` worktree an hour after its subagent
  finishes, runs Cut's `scripts/teardown` for it (its `cut_wt_<hash>`
  database goes), and leaves one Claude Code has locked for a running
  process. `git -C ~/code/cut worktree list --porcelain` shows each lock's
  reason, which on 2026-11-22 read `claude agent <id> (pid <n> start <s>)`:
  if Claude Code's names no pid, the sweep cannot tell a live lock, and
  relies on the transcripts and the folder's times;
- the sweep keeps Periphery's cache under 20 GB.
- slim simulators: on 2026-11-22 a `cut` session's new simulator was
  slimmed (157 of 173 services off in `simslim list`), `sim-full "<name>"`
  ended with *is full until it shuts down*, and `simslim doctor <udid>
  --requires push` passed. Still to see: after the apply that drops Claude
  Code's Nix `top`, `type -a top` in a `cut` session lists `/usr/bin/top`
  first, and a simulator the session boots shows about 75 processes in
  `simslim measure <udid>`. Xcode's parallel-testing clones show `slimmed`
  events in `simslim-watch.jsonl` while a test run with parallel testing on
  goes.

## Building and shipping apps

worker's sessions can sign any iOS, tvOS or visionOS app and send it to
TestFlight with one App Store Connect API key, through `app-store`
([`mac/app-store`](../mac/app-store/)). A new app needs no signing setup:
`xcodebuild -allowProvisioningUpdates` with the key registers its bundle ID
and capabilities, makes the certificates and makes the profiles. The
distribution certificate is cloud-managed, so its private key stays with
Apple. Both need the key's role to be Admin; Apple gives no lesser API key
the right to make certificates or to cloud-sign.

| Command | Does |
|---|---|
| `app-store testflight --project <p> --scheme <s>` | Checks the app has an App Store Connect record, archives, signs and uploads. `--workspace` for a workspace, `--platform tvOS` or `visionOS`, `--configuration` (Release by default), `--keep` to keep the archive. Xcode picks the next free build number itself |
| `app-store testflight --archive <path>` | Uploads an archive a failed upload kept |
| `app-store record <bundle-id>` | Says whether the app has a record. Exit code 3 if not |
| `app-store devices` | Lists the account's registered devices |
| `app-store register-device <name> <udid> [--mac]` | Registers a device, for development and ad hoc profiles |
| `app-store check` | Checks the key's files and calls the API with it |
| `app-store setup` | Stores the key and its IDs. Once, by hand, in `worker-shell` |

**What it can't do: make the app's record.** TestFlight takes a build only
for an app with a record in App Store Connect, and Apple's API has no way to
make one ([an open request on Apple's forums](https://developer.apple.com/forums/thread/851298),
read 2026-11-20). So `testflight` asks for the record before it builds. With
none, it registers the bundle ID, so the website offers it, and stops with
exit code 3 and the steps. A session reports that, and you make the record
(below).

**Only automatic signing.** It refuses a scheme with
`CODE_SIGN_STYLE = Manual`, or a `DEVELOPMENT_TEAM` other than the key's.
Cut signs manually with fastlane match and its own key, in its own CI; this
is for new apps.

**Not installing over the network.** A development build could go straight
to a phone on the same network with `xcrun devicectl`, but the Studio is on
its own VLAN and can start nothing towards the phone
([The network](#the-network)), and opening that is a network decision, not
this command's. TestFlight installs anywhere. `register-device` is there for
when a development or ad hoc build is wanted.

### The key

The key and its IDs are secrets, and this repo is public, so they are only
in worker's home:

| File | Holds |
|---|---|
| `~worker/.appstoreconnect/admin-key.p8` | The key. Not `private_keys/AuthKey_<Key ID>.p8`, where altool and notarytool look by themselves and would use it with no path on their command line |
| `~worker/.appstoreconnect/app-store.conf` | `key_id`, `issuer_id` and `team_id`, one `name=value` a line |

`setup` makes both readable by worker only, and every command refuses to run
while anyone else can read or write them. The token it signs for each run
lasts 10 minutes, goes to `curl` from a file, not on its command line, and is
never printed. Nothing a caller sets changes where it goes: the API host, the
key's folder and the paths of curl, xcodebuild and plutil are fixed in the
script, and curl runs with an empty environment and skips `~/.curlrc`.

It is a second key, not Cut's CI key, so either can be revoked alone.

**The risk.** Admin can do almost anything in the App Store Connect account:
make and revoke certificates, change apps' metadata and pricing, submit for
review, invite users and make other keys. And any code that runs as worker
can read the key file. That is more than the sessions' own commands: the
build phases and SwiftPM plugins of every project a session builds,
`npm` and `bun` install scripts, test suites, dev servers and MCP servers, in
every repo under `~/code`. None of those go past the auto mode classifier.

worker's `CLAUDE.md` tells sessions never to read, print, copy or send the
key, and the sessions' auto mode settings (`mac/claude`) tell the classifier
the same. That guards against a session talked into it, not against a
compromised dependency. So revoke the key (Users and Access, Integrations,
the key, Revoke) and make a new one whenever a repo a session worked in turns
out to have run hostile code, a session shows the key, or the Studio is lost.
Check Users and Access for users you did not invite too: an invitation made
with a stolen token outlives the key.

What would close this: the key owned by a separate user that runs nothing
else, with a sudo rule that lets worker run `app-store` as that user and
nothing more. It is not done here, because the upload's signing as another
user, and its keychain, are untested.

### Setting it up

Once:

1. **Make the key.** In [App Store Connect](https://appstoreconnect.apple.com),
   open Users and Access, then Integrations, then App Store Connect API, then
   Team Keys. Generate a key named `mac studio`, with the Admin role.
   Download its `.p8` file: Apple offers it only once. Note the Key ID beside
   it and the Issuer ID above the list. Keep the file in 1Password.
2. **Find the team ID.** It is under Membership details at
   [developer.apple.com/account](https://developer.apple.com/account).
3. **Store it on the Studio**, in worker's session:

   ```sh
   ssh -t ms hl shell
   ```

   ```sh
   app-store setup
   ```

   It asks for the Key ID, the Issuer ID and the team ID, then for the key
   itself. On the laptop, copy it with `pbcopy < ~/Downloads/AuthKey_<Key ID>.p8`,
   paste it, press Return and then Ctrl-D. Nothing shows while you paste, so
   the key never reaches the terminal, whose scrollback any session as worker
   could read. It ends with `app-store check`, which lists the account's
   apps. Then delete the download.

To replace the key, run `app-store setup` again: it replaces the old file.
To remove it, `rm -rf ~/.appstoreconnect` in `worker-shell`.

For each new app, when a session says it has no record:

1. **Make the record.** In [App Store Connect's apps](https://appstoreconnect.apple.com/apps),
   choose the + button, then New App. Pick the platforms, a name, a language
   and the bundle ID the session gave, and any SKU: only you see it.
2. **Let TestFlight hand you its builds.** In the app's TestFlight tab, add an
   internal group, add yourself, and turn on automatic distribution, so each
   upload reaches you without a step per build.
3. Tell the session to run `app-store testflight` again.

A build whose `Info.plist` lacks `ITSAppUsesNonExemptEncryption` waits at
*Missing Compliance* until someone answers the encryption question on the
website. `testflight` says so after the archive; setting
`INFOPLIST_KEY_ITSAppUsesNonExemptEncryption = NO` in the project avoids it
for an app that uses no encryption beyond Apple's.

### Still to test

CI runs `app-store` on Linux against stand-ins for curl and xcodebuild
([`mac/app-store/test-app-store`](../mac/app-store/test-app-store)), which
checks its tokens against openssl's verify and what it sends. None of this
has run on the Studio yet:

- `app-store setup` in `worker-shell`: the paste shows nothing, and
  `tmux -L claude-worker capture-pane -p -S - -t shell` afterwards holds no
  key; then `app-store check` listing the account's apps, which shows the
  token is right for Apple;
- that macOS's `/usr/bin/curl` runs with `env -i` and the flags `app-store`
  gives it;
- a first `app-store testflight` on a new app with no record: exit code 3,
  and the bundle ID in the developer account's Identifiers;
- after the record, the upload: xcodebuild makes a development certificate in
  worker's login keychain without a keychain prompt, cloud-signs, and the
  build shows in TestFlight. If `codesign` waits on a keychain prompt, no
  session can answer it; record the fix here;
- that worker's login keychain stays unlocked while worker's screen is
  locked, which signing needs;
- `app-store testflight --platform tvOS` on a tvOS scheme, and on an app
  with an App Clip or a watch app, where it must pick the host app;
- macOS's `/bin/bash` 3.2 runs the script: CI runs a newer bash.

## iOS CI

Phase 2, built after phase 3, on the `worker` user that
[Claude Remote Control](#claude-remote-control) sets up. Cut's iOS jobs run
here instead of on GitHub's billed macOS runners, each in a fresh Tart VM
that is deleted when the job ends. [`mac/ci`](../mac/ci/) sets it up.

| | |
|---|---|
| Runners | 2 slots, `dev.kylejs.ci-runner-1` and `-2`: Apple's licence allows two macOS VMs at once |
| Each VM | 6 CPU cores, 8 GB, cloned from the `cut-ci` image, on Softnet |
| GitHub | Just-in-time runners in cut-watch's `cut-mac` runner group, label `cut-mac`, registered by a GitHub App |
| Image | Cirrus Labs' Xcode image for the host's macOS, plus what Cut's `ios-setup` action and dead code check install |
| Package cache | One SwiftPM cache per slot, `~worker/Library/Caches/ci/swiftpm-<slot>`, kept between jobs |
| Health | Healthchecks.io's *Mac CI* check, pinged every 5 minutes by [`mac/ci/heartbeat`](../mac/ci/heartbeat) |

### How a job runs

Each slot is a launchd agent in worker's login session that runs
[`mac/ci/runner`](../mac/ci/runner) again each time it exits. One run clones
the image, boots the clone headless, and asks GitHub for a just-in-time
runner config, which the runner in the VM uses to take one job. When the job
ends, the runner deletes the VM and the runner. A slot that can't work (no
image, a full disk, GitHub refusing the App) logs the error, waits 2
minutes and tries again.

Before the runner starts, the VM copies the slot's SwiftPM cache onto its
own disk, and after the job it copies its cache back. Tart shares the folder
with the VM, which sees it at `/Volumes/My Shared Files/swiftpm`. The copy,
not the share, is what the job uses, so git never works over the share. The
next job's package resolve then fetches only what changed, where an empty
cache cloned all 14 of Cut's packages (about 1.2 GB) in every job. GitHub's
`actions/cache` can't do this here: it restores `~` paths into the runner's
folder, so Cut's workflows skip it on the Mac Studio. A failed copy only
costs the job its cache: it resolves from scratch, as before.

The VMs run in worker's session, not as a daemon: Apple's Virtualization
framework refuses to run in one. So after a restart nothing runs until
worker logs in ([Logging worker in](#logging-worker-in)).

Softnet lets a VM reach the internet and the bridge's gateway, which is the
Mac Studio itself, and nothing else: not the LAN, not the tailnet, not the
other VM. A pull request's code runs in the VM, so this is the boundary
between it and the house. Softnet needs root for its network interface,
and the Nix store can't hold the setuid bit Tart would set, so a sudoers
rule lets worker run that one binary as root.

### Fallback to GitHub's runners

Cut's workflows read the *Mac CI* check before each iOS job. Up sends the
job to `cut-mac`; down or late sends it to GitHub's `xcode-27` runner, under
the label gate Cut had before. Cut's repository variable `IOS_RUNNER`
overrides the check: `mac` or `github`. While the Mac Studio is up, iOS
checks run on every push that touches `ios/` or the schema, and each merge
to main deploys to TestFlight. While it is down, the 2-hourly scheduled
deploy covers TestFlight. Cut's `ios/AGENTS.md` has the workflow side.

The heartbeat pings `/fail`, which marks the check down at once, when no
slot can take a job. One bad slot of two still pings success, with the
problem in the ping's body. Silence marks it down after the grace period:
the Mac Studio off, still locked, or worker not logged in.

### Setting it up

Once, in this order. Steps 1 to 3 are on GitHub and Healthchecks.io, as Kyle;
the rest in `ssh -t ms worker-shell`.

1. **Make the GitHub App.** In cut-watch's settings, Developer settings,
   GitHub Apps, New GitHub App: name `cut-mac-runners`, any homepage URL,
   webhook off, and one permission, *Organization: Self-hosted runners:
   Read and write*. Nothing else: the App registers runners and can't read
   code. Only on this account. Create it, note its client ID, and generate
   a private key, which downloads a `.pem`. Install the App on cut-watch.
   GitHub offers only *No repositories*, because the App has no repository
   permission. That is right: the runner group in step 2 picks which
   repositories use the runners.
2. **Make the runner group.** In cut-watch's settings, Actions, Runner
   groups: `cut-mac`, with repository access set to `cut` alone and public
   repositories left off. If GitHub won't make a group on cut-watch's plan,
   use `Default`, restricted the same way, and set `group = "Default"` in
   `mac/ci/default.nix` and in Cut's runner choice.
3. **Make the *Mac CI* check** on Healthchecks.io: a 5-minute period and
   10 minutes' grace, slug `mac-ci`. For Cut's workflows, make a read-only
   API key in the project's settings and store it as Cut's Actions secret
   `HEALTHCHECKS_READ_KEY`. A read-only key can read check statuses and
   can't ping or change anything.
4. **Hand worker the App and the ping URL**, in `worker-shell`, one
   command at a time. `worker-shell` is zsh, where `read -p` means something
   else, so these print their own prompt. The next two each ask for a value
   and save it when you press Return:

   ```sh
   mkdir -p ~/.config/ci
   ```

   ```sh
   (umask 077; printf 'client ID: '; read -r v && printf '%s\n' "$v" > ~/.config/ci/github-app-client-id)
   ```

   ```sh
   (umask 077; printf 'ping URL: '; read -r v && printf '%s\n' "$v" > ~/.config/ci/ping-url)
   ```

   The key is many lines, so this one waits until Ctrl-D. Open the `.pem`
   on the laptop, paste all of it, then press Return and Ctrl-D:

   ```sh
   (umask 077; cat > ~/.config/ci/github-app.pem)
   ```

   This should print `-----BEGIN RSA PRIVATE KEY-----`:

   ```sh
   head -n 1 ~/.config/ci/github-app.pem
   ```

   Delete the downloaded `.pem` from the laptop once the first job has run.
   A new key is a click in the App's settings, where the old one can be
   revoked.
5. **Build the image**, still in `worker-shell`:

   ```sh
   build-image
   ```

   It pulls about 60 GB, boots a copy, runs
   [`mac/ci/image-setup`](../mac/ci/image-setup) in it and saves it as
   `cut-ci`. It picks the Cirrus Labs image from the host's macOS (26 is
   Tahoe, 27 is Golden Gate), because a guest can't be newer than its host.
   `build-image --source <image>` names another.
6. **Start the runners:**

   ```sh
   for a in ci-runner-1 ci-runner-2 ci-heartbeat; do launchctl kickstart -k "gui/$(id -u)/dev.kylejs.$a"; done
   sleep 120; tail -n 5 ~/Library/Logs/ci-runner-1.log ~/Library/Logs/ci-heartbeat.log
   ```

   Each runner log ends with the runner waiting for a job, the heartbeat's
   with `ok`, and cut-watch's runner group lists two idle runners.
7. **Check the VM can't reach home**, from `worker-shell` while slot 1 waits
   for a job:

   ```sh
   tart exec cut-ci-1 curl -sS -o /dev/null -w '%{http_code}\n' https://github.com   # 200
   tart exec cut-ci-1 nc -z -G 3 192.168.3.2 22     # fails: the NAS
   tart exec cut-ci-1 nc -z -G 3 192.168.40.2 22    # fails: the Mac Studio's LAN address
   tart exec cut-ci-1 nc -z -G 3 100.100.100.100 53 # fails: the tailnet
   ```

   `-G 3` gives up on the connection after 3 seconds. macOS's `nc` ignores
   `-w` until a connection opens, so without it each check hangs instead.

8. **Run a job.** Set Cut's `IOS_RUNNER` variable to `mac` and add the
   `run-all-tests` label to a Cut PR that touches `ios/`. Its iOS jobs run
   on `mac-studio-1-…` and `mac-studio-2-…`. Then delete the variable, so
   the check decides.
9. **Done when** a Cut PR's iOS build and tests pass on the Mac Studio, a
   merge to main deploys to TestFlight from it, and with the runners
   stopped, the *Mac CI* check goes down within 15 minutes and the next
   push runs on GitHub's runner. Stop and start them in `worker-shell`:

   ```sh
   for n in 1 2; do launchctl bootout "gui/$(id -u)/dev.kylejs.ci-runner-$n"; done
   for n in 1 2; do launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/dev.kylejs.ci-runner-$n.plist; done
   ```

### Day to day

Logs, as `kylejs`:

```sh
sudo tail -n 50 ~worker/Library/Logs/ci-runner-1.log  # one slot's jobs
sudo tail -n 50 ~worker/Library/Logs/ci-vm-1.log      # Tart and Softnet, timestamped
sudo tail -n 50 ~worker/Library/Logs/ci-net-1.log     # Softnet CPU and the Mac's load
sudo tail -n 20 ~worker/Library/Logs/ci-heartbeat.log # "ok", or the problems
```

- **A download in a job crawls now and then** (seen 2026-11-14: a package
  clone took 347 seconds that took 8 a minute earlier in the same VM, while
  the Mac Studio cloned it in 6). `ci-net-<slot>.log` samples Softnet every
  30 seconds; match the slow step's time in the job log against it.

- **An apply waits for a slot's job before restarting it.** An apply
  restarts a slot only when it changes that slot's plist: a change to
  `mac/ci/runner` or the CI settings, or a flake update that moves Tart or
  Softnet. Before it does, it drains the slot, one slot at a time, so the
  other keeps taking jobs ([`mac/ci/drain`](../mac/ci/drain)):

  1. It writes `~worker/Library/Caches/ci/drain-<slot>`. While that file
     stands, the slot takes no new job. `build-image` keeps its own request
     beside it, `drain-<slot>.build-image`, so neither drops the other's.
  2. An idle runner is removed from GitHub and its VM stopped. A runner with a
     job finishes it.
  3. Once the slot is parked, the apply restarts it on the new config and
     removes the file.

  The apply prints a line when it starts waiting and one a minute after.
  After 45 minutes, longer than a deploy's 40, it gives up: the slot keeps
  its old config and takes jobs again, and the apply says to run it again.
  `--wait-minutes <n>` changes the wait. `--no-wait` restarts the slots at
  once and kills their jobs, as every apply did before 2026-11-14. Use it
  only when Cut's Actions page shows no job on the Mac Studio.

  The request ends by itself 5 minutes after the wait would have, so an apply
  cut off mid-wait (Ctrl-C, a dropped SSH session) leaves no slot parked for
  long. A dropped session also stops the activation halfway, so run long
  applies in tmux. To release a slot by hand, as kylejs or worker:

  ```sh
  hl drain release 1
  ```

  `hl status` shows a held slot, who holds it and until when.

  The heartbeat counts a parked slot as unhealthy, with a note, so while both
  are parked Cut's jobs go to GitHub.
- **The image moves with Xcode.** After `mac/xcode-version` changes and
  `mac/bootstrap` runs, run `build-image` in `worker-shell`. Run it too after
  an apply that changes a tool version the image bakes in (XcodeGen, the
  Apollo CLI and Periphery, in `mac/ci/default.nix`). Monthly, with the Xcode
  chore, run it again for the image's macOS and tool updates. Jobs already
  running keep their clones; new jobs get the new image. macOS runs at most
  two macOS VMs, and each slot keeps one booted while it waits for a job, so
  `build-image` borrows slot 2's: it drains the slot (waiting up to 150
  minutes for a job to finish, and then for the slot's VM to be deleted),
  builds in its place, and lets it take jobs again when it ends. Slot 1
  takes Cut's jobs meanwhile. Its request is a 10-minute lease it renews
  while it runs, so a long setup keeps the slot. If `build-image` is killed
  outright (`kill -9`), the process renewing the lease stops `cut-ci-new`
  and releases the slot. Killed while still waiting for slot 2's job, it
  leaves the slot parked until its request runs out, up to 155 minutes; to
  release it sooner, in `worker-shell`:

  ```sh
  ci-drain release dev.kylejs.ci-runner-2 build-image
  ```
- **To reset a slot's package cache**, delete its folder. The next job on
  that slot resolves from scratch and fills it again. The runner deletes it
  itself when a copy back is cut short.

  ```sh
  sudo rm -rf ~worker/Library/Caches/ci/swiftpm-1
  ```

- **A job is stopped after 2 hours**, longer than any of Cut's iOS timeouts,
  so a hung VM can't hold a slot.
- **The runners stop at 50 GB free** and the heartbeat fails, which sends
  Cut's jobs to GitHub until the disk is cleared.

### Accepted risks

- worker may run Softnet as root with arguments of its choosing. Anything
  running as worker, Remote Control sessions included, could use that to
  give a VM the LAN.
- A VM can reach the bridge's gateway, the Mac Studio itself, where SSH takes
  keys only and Screen Sharing takes a password.
- The App's private key is a file in worker's home, readable by worker's
  Remote Control sessions. It can register and remove cut-watch's runners,
  and nothing else.
- A job can change its slot's package cache, which the next job on that
  slot starts from, a TestFlight deploy included. `Package.resolved` pins
  each package to a commit, so a changed clone can't change what is built,
  but a changed git config in one could run a command in the next job. Only
  branches in Cut's private repository run here.

### Still to test

Tested on the Mac Studio on 2026-11-13: the image built from the Golden
Gate image with the Metal toolchain and iOS 27.1, `tart exec -i` hands each
runner its config, Softnet starts through the sudo rule, and the step 7
checks pass. Cut pull request 1024's iOS build and tests then passed, one on
each slot. Nothing is left to test before 2026-11-21, the end of the return
window.

Not yet tested: draining. In `worker-shell`, `ci-drain wait
dev.kylejs.ci-runner-1 5` on an idle slot should return within a minute,
with `removed idle runner` in `ci-runner-1.log`; `ci-drain release
dev.kylejs.ci-runner-1` then lets it take jobs again. While a job runs on a
slot, the same wait on it should print a line a minute and return once the
job ends, with the job passing.

Not yet tested: the package cache. After the apply that adds it, each slot's
second job should log `SwiftPM cache copied in` with a size near 1.2 GB, and
its *Resolve SPM dependencies* step should take seconds, not a minute.

## Linux containers

Phase 4. [`mac/colima`](../mac/colima/) runs [Colima](https://github.com/abiosoft/colima)
as `worker`: one Linux VM that serves the Docker API, started at worker's
login by a launchd agent, `dev.kylejs.colima`, and stopped when the agent
stops. worker's sessions use it for the services a repo runs with Docker
Compose. Nothing moves off the NAS in this phase.

It was added after 2026-11-13, when a Cut session found no Docker and
installed Postgres and Redis on the host instead. Every later session then
shared those, at the version that one agent picked, and the next agent might
have installed different ones.

| | |
|---|---|
| VM | `vz`, 4 CPUs, 4 GB, a disk that grows to 60 GB, in `~worker/.colima` |
| Mounts | `~worker/code`, writable, over virtiofs. A container can bind-mount only files under it |
| Socket | `~worker/.colima/default/docker.sock`. `DOCKER_HOST` points the servers, their sessions, `worker-shell` and the heartbeat at it |
| Tools | `colima`, and `docker` with its `compose` and `buildx` plugins, from Nix |
| Log | `~worker/Library/Logs/colima.log` |

**One Docker for every session.** All of worker's sessions share this one
engine. Cut's `scripts/bootstrap` starts one set of Postgres, Redis and S3
containers for all its worktrees, and gives each worktree its own database,
Redis key prefix and test databases. Two repos whose Compose files publish the
same host port can't run at once; the second gets *port is already
allocated*. Sessions are told never to install a database on the host, and
never to stop or prune containers or volumes they did not make
([`any-AGENTS.md`](../mac/claude/any-AGENTS.md) and Cut's `AGENTS.md`).

**CPU and memory.** Four vCPUs cap what the VM can take from a host that is
already oversubscribed; an idle VM takes almost no CPU, because a vCPU runs
on the host only while the guest has work. Memory the guest has touched may
not go back to macOS until the VM stops, so count the 4 GB as used. The VM
does not stop when no session needs it: a stopped VM would add a boot of up
to a minute to the next bootstrap, for a saving of almost no CPU. The usage
email counts Colima's VM apart from the CI VMs: ps shows both as the same
Virtualization process, and Lima's host agent marks Colima's.

**Restarts.** The first start, at worker's first login after the apply that
adds it, downloads the VM image (about 300 MB) and takes a few minutes. After
that a login boots the VM in under a minute. An apply that changes the
agent's plist, which a new Colima version in the monthly flake update does,
restarts the VM: running containers stop, and their volumes stay. Cut's
bootstrap starts them again. A VM left running by a crash is stopped and
started again by the agent, so the agent always owns the VM it stops.

To check it, in `worker-shell`:

```sh
colima status
```

```sh
docker ps
```

### Tools installed outside the Nix config

worker has no dotfiles and gets every tool from the Mac's Nix config. A tool
in worker's home was most likely installed by a session, and a later session
can then run it at a version no repo chose. The heartbeat reports:

- a Nix profile (`nix profile install`, `nix-env -i`), by package name, with
  the command that removes it; for a profile `nix-env` made, by command;
- commands in the folders installers put them in: `~/.local/bin`, `~/bin`,
  `~/.bun/bin`, `~/.npm-global/bin`, `~/.cargo/bin`, `~/go/bin`,
  `~/.deno/bin`, `~/.mint/bin`, `~/Library/pnpm` (not its package store),
  `~/.local/share/gh/extensions`, `~/Library/Python/*/bin` and
  `~/.gem/ruby/*/bin`;
- a `~/periphery` folder, where Cut's CI workflow installs Periphery: on the
  host, `periphery` comes from the Nix config. It also notes when Cut's main
  pins another Periphery release than the one on PATH
  ([Keeping it current](#keeping-it-current));
- a version manager's folder: `~/.nvm`, `~/.volta`, `~/.fnm`, fnm's folder in
  `~/Library/Application Support`, `~/.nodenv`, `~/.pyenv`, `~/.rbenv`,
  `~/.asdf`, `~/.local/share/mise`, `~/.rustup` or `~/.sdkman`;
- a shell startup file (`~/.zshrc`, `~/.bash_profile`, fish's config and the
  like), and an `~/.npmrc` that sets npm's global `prefix`. These are what
  put the folders above on `PATH` for every later session;
- a LaunchAgent in `~/Library/LaunchAgents` not named `dev.kylejs.*`, since
  the setup installs only those.

A tool that a repo's own scripts install where only they run it is expected.
Cut's `ios/scripts/bootstrap.sh` puts `apollo-ios-cli` in `~`, which none of
these checks looks at. If a check flags a path the setup or a repo needs, add
it to `allowed_tools` near the top of the tools checks in
`mac/claude/heartbeat`, one path a line, relative to worker's home.

These are housekeeping, not an outage. The first run that sees a new one
pings `/fail`, so Healthchecks.io emails once. Later runs ping success with
the list in the body, which stays in the check's event log until the tool is
gone. A lasting `/fail` would hide the next real failure, since
Healthchecks.io emails only when the check changes state.

If a session needs the tool, add it to `environment.systemPackages` in
`mac/hosts/mac-studio.nix` instead. Then remove the copy in worker's home. A
Nix profile goes with the command the heartbeat gives, or step 7 below. A
folder of commands, a version manager's folder or a LaunchAgent, once you
have looked in it:

```sh
sudo rm -rf ~worker/<path>
```

A LaunchAgent may be loaded, so unload it first, as step 4 below does. A
shell startup file may hold more than one installer's lines; read it before
you remove it.

idb was the first: on 2026-11-13 a session put `idb-companion` 1.1.8 in
worker's Nix profile and the client in `~worker/Library/Python/3.9` with
`pip install --user`. Once an apply has put [`mac/idb`](../mac/idb/)'s on
PATH, remove both copies.

A companion the old client started keeps running after its session ends, and
the new client reuses it for that simulator. List any that are running:

```sh
pgrep -lu worker idb_companion
```

Stop them, and remove the state file that records them:

```sh
sudo pkill -u worker -x idb_companion
```

```sh
sudo rm -f /tmp/idb/state
```

Remove the old companion from worker's Nix profile:

```sh
sudo -iu worker nix profile remove idb-companion
```

Look at what pip put in Apple's Python folder for worker:

```sh
sudo ls ~worker/Library/Python/3.9/bin ~worker/Library/Python/3.9/lib/python/site-packages
```

If it holds only idb and its dependencies (grpclib, h2, hpack, hyperframe,
multidict, protobuf, aiofiles), remove the folder:

```sh
sudo rm -rf ~worker/Library/Python/3.9
```

### Cleaning up a host install

If the heartbeat reports a database running on the host, it names the
process, its user and its pid. On 2026-11-13 it was Postgres and Redis,
installed by a Cut session as worker. As `kylejs`:

1. List them, with the path each one runs from. A path under `/nix/store`
   came from a Nix profile, one under `/opt/homebrew` from Homebrew, one under
   `~worker` from a download:

   ```sh
   ps -axo user,pid,command | grep -E '[p]ostgres|[r]edis-server|[v]alkey-server|[m]inio|[r]ustfs|[m]ysqld|[m]ariadbd|[m]ongod'
   ```

2. See whether a launchd agent starts them again. Anything not starting
   `dev.kylejs.` was added by hand or by an agent:

   ```sh
   sudo ls ~worker/Library/LaunchAgents
   ```

3. See what worker installed with Nix:

   ```sh
   sudo -iu worker nix profile list
   ```

4. Unload each launchd agent step 2 found, by its file name, before you stop
   the process: an agent with `KeepAlive` starts a killed process again:

   ```sh
   sudo launchctl bootout gui/$(id -u worker) ~worker/Library/LaunchAgents/<file>
   ```

5. Remove that agent's file:

   ```sh
   sudo rm ~worker/Library/LaunchAgents/<file>
   ```

6. Stop each process still running, with its pid from step 1. For Postgres,
   the oldest `postgres` process is the server, and stopping it stops the
   rest:

   ```sh
   sudo kill <pid>
   ```

7. Remove each package step 3 listed, by its name there:

   ```sh
   sudo -iu worker nix profile remove <name>
   ```

8. Find the data they left:

   ```sh
   sudo find ~worker \( -name postgresql.conf -o -name dump.rdb -o -name appendonlydir -o -name appendonly.aof \) -not -path '*/.colima/*' -not -path '*/Library/*'
   ```

   A `postgresql.conf` sits at the top of a Postgres data folder: delete
   that folder. Redis writes `dump.rdb` and `appendonlydir` in the folder it
   was started from, which can be a worktree with work in it, so delete only
   those, never the folder around them. All of it is local dev data: Cut's
   real data is in production.

Then confirm the heartbeat log reads `ok`:

```sh
sudo tail -n 5 ~worker/Library/Logs/claude-rc-heartbeat.log
```

### Still to test

- The first start downloads the image and boots a `vz` VM as worker, under
  launchd, with no terminal: `colima status` in `worker-shell` reports it
  running.
- `docker compose up` in a Cut worktree, through `./scripts/bootstrap`,
  starts Postgres, Redis and S3, and Cut's backend tests pass against them.
- After `launchctl kickstart -k gui/$(id -u worker)/dev.kylejs.colima` as
  root, `colima.log` shows the VM stopping and starting, and `colima status`
  reports it running.
- The heartbeat reports a host install: `sudo -iu worker nix shell nixpkgs#redis --command redis-server --port 6390`
  for one run, then stop it.
- The first heartbeat after the apply pings `/fail` for any tool worker
  already has outside the Nix config, and the next one pings success with
  the list; Postgres and Redis from 2026-11-13 may show as a Nix profile. A
  path there that the setup needs, rather than one a session installed, goes
  in `allowed_tools` in `mac/claude/heartbeat`.
- idb from `mac/idb` drives a simulator: in `worker-shell`, `idb list-targets`
  lists the simulators, and `idb ui describe-all --udid <udid>` on a booted
  one prints its screen's elements. Only the layout was checked off the Mac.
- `periphery version` in `worker-shell` prints `3.9.12-agoodkind.1`, and
  `ios/scripts/periphery-scan.sh --build` in a Cut checkout runs a scan. The
  apply itself runs `periphery version` when it builds the package.
- A session's simulator recording, with the recipe in worker's
  `~/.claude/CLAUDE.md` ("Screen recordings"), stops cleanly on
  `kill -INT "$(cat rec.pid)"` and `ffprobe` reads its length. A shell's
  `&` starts a child with SIGINT ignored, so if sessions start background
  tasks that way, `simctl` would have to catch SIGINT itself. Only the
  ffmpeg commands were run off the Mac, on Linux with the same package.

## Usage and cost

Whether the Mac Studio pays its way, from what it actually does.
[`mac/usage`](../mac/usage/) records it and prices it against hosted Macs.
Added on 2026-11-14 to decide before the return window closes on 2026-11-21.

```sh
mac-usage report
```

`--since 2026-11-17` narrows it; `--json` prints the numbers for a script.
It starts at the Mac Studio's first CI job, 2026-11-14 at 02:38 UTC.
Any user can run it.

What it records:

| | Where from | Kept in | Runs |
|---|---|---|---|
| CI jobs, minutes, queue wait, runner | GitHub's Actions API for every cut-watch repo, with worker's `cut-watch` token (Actions: read) | `/Users/Shared/mac-usage/state.json` | worker agent `dev.kylejs.mac-usage`, every 15 minutes while worker is logged in |
| Agent sessions: active and open hours, prompts, tool calls, and how many ran xcodebuild, Cut's build scripts or a simulator command | Worker's `~/.claude/projects/*/*.jsonl`: timestamps, the worktree, and the name of each tool call and whether a command runs Xcode or a simulator | the same file | the same agent |
| Each session's Xcode builds and test runs: when, how long, failed or not | Xcode's `LogStoreManifest.plist` index in the session's DerivedData (Cut's `ios/.derivedData`), or Xcode's own DerivedData for its project | the same file | the same agent |
| Load, CPU, memory, swap and swap writes, memory pressure and free memory percentage, free disk, booted simulators, and the CPU and memory footprint of the VMs, Claude, Xcode and the simulators | `top`, `vm_stat`, `sysctl`, `ps` | `/var/log/mac-usage/host-<day>.jsonl` | root daemon `mac-usage`, every minute |
| Each session's Xcode processes and booted simulators, xcodebuild's parallel-testing clones included, and their CPU | `ps`, with `lsof` for an Xcode process whose command line names no folder, the simulator's `device.plist`, and each process's parent for the simulator's own processes | the same file | the same daemon |
| Chip power and thermal pressure | `powermetrics` | the same file | the same daemon |

How it prices them, all constants at the top of `mac/usage/mac-usage`:

- **CI at `xcode-27`'s rate**, the runner Cut falls back to: $0.102 a
  minute, with no free minutes. Its job logs show the `xcode-27-arm64`
  image, and GitHub's only arm64 larger runner is the M2 Pro. That is
  inferred: the org's hosted-runner settings need an org admin to read, so
  check them once and correct the constant if they differ.
- **Each Mac Studio job is priced at what the same job took on
  `xcode-27` with the same outcome**, from its runs since 2026-11-01, by
  job name: a success from its successes, a failure or timeout from its
  failures and timeouts. At their mean, so slow runs count, like a deploy
  waiting on TestFlight, or at their median when there are under 5 such
  runs, so one outlier can't set the price. A job with no such history,
  or cancelled, is priced at its own minutes. The CI section shows the
  median, mean and count behind each job's price.
- **Cut's agent sessions that used Xcode or a simulator at hosted Macs'
  price, as a range**: one Mac whenever any of them is active at the low
  end, one per session at the high end. A session that never built, ran
  tests or touched a simulator would run on Linux, so it isn't priced; the
  report gives every Cut session's range beside it for comparison. Namespace's smallest macOS machine, 4 vCPU and 7 GB, at
  $0.06 a minute pay as you go (namespace.so pricing, read 2026-11-16).
  Cloud sessions run on Linux, so they can't build Cut in Xcode or run the
  simulator. The rate is a floor: Xcode builds may need a larger machine,
  and the 6 vCPU, 14 GB one is $0.09. A session is active while its
  transcript has entries no more than 5 minutes apart, while its Xcode
  processes run, and during its builds and test runs, so a long build
  counts. The time between turns, which a hosted Mac would still bill,
  doesn't. A session is its worktree, with its subagent transcripts
  counted as part of it.
  Sessions on the `any` server are left out, since its repos build on
  Linux.
- **Four paybacks**, $2,909 net over the value per month less power: all
  the CI the Mac Studio ran, only the jobs GitHub's runners would have run
  anyway, and each of those with Cut's agent sessions added, as a range.
  While the Mac Studio is up, Cut runs iOS CI on every push to an iOS pull
  request and deploys every merge; before, iOS ran only on PRs labelled
  `run-all-tests` or `run-lite-tests`, IPA builds and scheduled deploys.
  The second figure is closer to money saved on CI alone, and the far end
  of the fourth's range is the cautious figure with sessions. Extra runs a
  PR would have needed on GitHub, and faster answers, are in none of them.
- **CI, agent sessions and the pace share one start**: the first Mac
  Studio job, or `--since` if later. The report gives the hours with at
  least one session active, and session-hours in all.

The "Agent sessions" section shows where the time went, not only how long
sessions were open:

- **Open against active hours**: open runs from each conversation's first
  entry to its last, so the gap is waiting for the next prompt, or idle. A
  worktree that holds several conversations days apart is open only during
  each.
- **Xcode**: builds and test runs from Xcode's logs, with their hours and
  failures, and the hours and CPU-hours of each session's Xcode processes.
  It says what share of the host's Xcode CPU was traced to a session; the
  rest ran in no session's folder. A command counts as running Xcode or a
  simulator only where it starts one, so reading Cut's build scripts does
  not.
- **Simulators**: simulator-hours booted, two at once counting double, and
  their CPU: launchd_sim, the apps installed in the simulator, and the
  simulator's own processes, such as SpringBoard, which it starts. It says
  what share of the host's booted simulator time was traced to a session.
  Cut's test script names its simulators "Cut Tests ...", so they are
  traced through the xcodebuild run that names their UDID; others through
  the " @" and session ID worker-CLAUDE.md has sessions end a name with.
- **The busiest sessions**, a row each, by active hours.
- CI VMs' builds and simulators run inside the VMs, so none of this counts
  them.
- **Power** at $0.35 a kWh from the chip's CPU, GPU and ANE power, so a
  floor under the power at the wall.
- The decision record expected about $119 a month at GitHub's standard
  rate after free minutes. The report shows it for scale only.

Whether 64 GB and 1 TB are enough, for the keep-or-return call, is the
report's "Memory and disk headroom" section. It gives each a verdict, enough,
tight or short, with the reason. Then it shows what was running at the worst
pressure and at the most memory used, and one row per day with that day's CI
VMs, Claude processes and booted simulators beside the peaks:

- **Memory goes by pressure and swap writes, not GB used.** macOS keeps
  memory full of cache and compresses before it swaps, so used memory near
  64 GB is normal. Short is any critical pressure, a day with 30 minutes or
  more at warning or worse, or 10 GB a day or more swapped out. Tight is any
  warning, or 1 GB a day or more swapped out. Per day, so quiet nights can't
  water down an hour of pressure while CI and agents run.
- **Disk goes by the lowest free space and its trend**, against
  `mac/claude/heartbeat`'s thresholds. Short is under 75 GB, or a straight
  line through the samples reaching 75 GB within 30 days. Tight is under
  150 GB, or reaching 150 GB within 90 days.
- **A verdict says "so far, too early to call" until its readings span 3
  days**, with samples for 90% of the minutes. Swap writes, the free memory
  percentage, the disk's size, booted simulators and restarts start at the
  apply that adds them, so the memory verdict stays provisional for 3 days
  after it; memory, pressure and free disk go back to the daemon's first
  sample.
- **The breakdown of what held memory is by footprint**, the figure
  Activity Monitor's Memory column shows: each process's own pages,
  compressed ones included. Before 2026-11-17 it came from resident memory,
  which counts shared pages in every process that maps them. Each booted
  simulator runs dozens of processes that map the same system libraries, so
  on 2026-11-17 the simulators read 78 to 80 GB, more than the 49 GB used.
  The same report counted none booted; the count now also takes a bare
  `launchd_sim`, which it missed before, though whether that was the cause
  there is untested. Samples from before then still show resident memory,
  and say so.
- **How to read memory.** Used is what apps, the kernel and the compressor
  hold, and runs high on a Mac by design. Compressed is what macOS squeezed
  instead of swapping. Pressure is macOS's own call on whether memory is
  short, and the one the heartbeat alerts on. Beszel's chart shows used
  memory only, averaged over each point unless it is set to *Max*, so a spike
  that pushed pressure to warning can read flat there.

What to keep in mind when reading it:

- **Check the data's age first.** The report says how old the CI data is
  and when the last host sample was. With CI data over an hour old it gives
  no payback, and says the collector has stopped. The pace runs to the last
  collect, so stale data can't move it.
- **The pace needs days.** The report says so under 14 days of CI. One busy
  evening extrapolates to a large month.
- **CI is backfilled from 2026-11-01**, and counted from the Mac Studio's
  first job. A job cancelled before it reached a runner has no minutes,
  since GitHub bills none. Host and power data start at the apply that adds
  the daemon: nothing before it can be recovered.
- **Agent hours need worker logged in** for the agent to run. It catches up
  from its last run, and keeps what it counted from a transcript that
  Claude Code later deletes.
- Once a day the collector lists every run since 2026-11-01 again, to catch
  re-runs of old runs. A day with 1,000 runs or more in a repo, the most the
  API returns, shows as a problem in the report.
- From the transcripts it keeps timestamps, the worktree, and counts:
  prompts, tool calls, and calls that run Xcode or a simulator. It reads a
  command only to count it, and keeps no text. From `ps` it keeps process
  group totals and session IDs, never command lines. From Xcode's logs it
  keeps when each run started, how long it took and its status, not its
  title. The host files grow about 1 MB a day.
- **The per-session tracing starts at the apply that adds it.** Xcode's
  logs and the transcripts go back as far as they are still on disk;
  sweep-sessions deletes a session's DerivedData 2 hours after its last use.

Logs:

```sh
tail -n 20 /var/log/mac-usage.log
```

```sh
sudo tail -n 20 ~worker/Library/Logs/mac-usage.log
```

## Monitoring

The NAS's monitoring hub watches the mac studio as it watches the NAS: its
dashboard, alerts when the mac studio is off or its disk passes 90%, and an
email every Monday, `[homelab] mac studio weekly`, with what to watch or act
on and why. What it judges, and what it leaves to the heartbeats, is in
[`monitoring/README.md`](../monitoring/README.md#the-mac-studio).

[`mac/beszel`](../mac/beszel/default.nix) runs Beszel's agent as a launchd
daemon, `org.nixos.beszel-agent`, as `nobody`. The hub on the NAS connects to
it at `192.168.40.2:45876`, and it answers only the hub's key, which
`mac/keys/beszel-hub.pub` holds. Until that file holds a key, an apply warns
and installs nothing.

### Turning it on

Four steps, in this order, so the hub's Status alert does not fire while the
agent is not up yet.

1. **The hub's key.** On the NAS, in the checkout:

   ```sh
   grep '^MON_HUB_KEY=' monitoring/.env
   ```

   Put the part after `=` in `mac/keys/beszel-hub.pub` on its own line, and
   merge it. It is a public key.

2. **The gateway.** In UniFi, add a policy *NAS to Mac Studio monitoring*:
   from `192.168.3.2` to the *Mac Studio* zone, Allow TCP 45876, with
   replies, ahead of the zone's block. Then add it to [The
   network](#the-network) and [`unifi.md`](unifi.md).

3. **Apply on the mac studio:**

   ```sh
   ssh ms
   ```

   ```sh
   ~/homelab/mac/apply --pull
   ```

4. **Add it to the hub.** On the NAS, set `MON_MAC_STUDIO_HOST=192.168.40.2`
   in `monitoring/.env`, then:

   ```sh
   homelab deploy monitoring
   ```

   ```sh
   homelab setup monitoring
   ```

   Setup adds the `mac-studio` system and its two alerts, signed in by your
   Tailscale login, so it asks for no password. The hub's systems page shows
   it up within a minute.

To see the email before Monday, on the NAS, from the checkout:

```sh
cd monitoring
```

```sh
docker compose -p monitoring run --rm --no-deps weekly-report -dry-run -only mac-studio
```

The first two-hour sample lands two hours after the hub connects.

### When the hub cannot reach it

The hub's Status alert emails after 10 minutes. On the mac studio, as
`kylejs`:

```sh
sudo launchctl print system/org.nixos.beszel-agent
```

```sh
sudo tail -n 20 /var/log/beszel-agent.log
```

A listen error at boot is the LAN address not being up yet; launchd restarts
the agent 30 seconds later. If the agent runs, the hub's side is in
`homelab logs monitoring` on the NAS. A restored hub has a new key: repeat
step 1, replacing the old key line rather than adding a second, then apply.

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
the Studio to Trusted. Connect Screen Sharing to the tailnet name,
`vnc://mac-studio.tail3df177.ts.net`, not to the Studio in Finder's sidebar.
The sidebar entry can also be a cached record of an address the Studio no
longer has. `vnc://mac-studio.lan` works only while the laptop's Tailscale is
disconnected: while it is connected, the NAS's route carries `192.168.40.2`,
and UniFi lets the NAS in on TCP 22 alone ([FileVault](#filevault)).

## Keeping it current

- `hl apply` (`mac/apply --pull`) pulls this checkout and the dotfiles
  checkout, then applies. After the pull, `mac/apply` runs again from the
  pulled copy, so a change to `mac/apply` itself takes effect in that run. A
  pull that can't fast-forward stops before anything is applied.
  `hl apply --no-pull` applies the checkouts as they stand, as `mac/apply`
  does on its own. `hl apply build` pulls, then builds without switching.
  `hl update` pulls both without applying. An apply that changes a CI
  runner slot waits for its job first ([Day to day](#day-to-day)).
- A dotfile change is a commit to kylejs-macOS-bootstrap, never to this repo.
  Guard a tool not every Mac has with `type -q` in fish.
- A flake only sees files git tracks. `mac/apply` warns when `mac/` has
  uncommitted changes, because a new file nobody staged is silently absent.
- Xcode moves when Cut's CI does: change `mac/xcode-version` in a PR, then run
  `~/homelab/mac/bootstrap` again on the Studio, which pulls, installs the
  new version and moves the link. Remove the old one with `xcodes uninstall <version>`.
  Then, once the new Xcode has opened once, delete the simulators no installed
  runtime supports, as worker in `worker-shell`: `xcrun simctl delete unavailable`.
  The sweep leaves this to you, because the same command run at the wrong
  moment deletes the devices Cut's tests copy from.
- Monthly, with the Xcode chore, update the lock on a branch, so
  `check-mac.yml` evaluates it before the Studio applies it. On the Studio,
  in an `ssh -A` session for the push:

  ```sh
  cd ~/homelab && git switch -c flake-update
  nix flake update --flake ./mac && mac/apply build
  git commit -am "mac: update flake.lock" && git push -u origin flake-update
  ```

  `mac/apply build` here, not `hl apply build`: the new branch has no
  upstream yet, so `hl apply`'s pull would stop it. Open the PR on GitHub
  and merge it once CI is green. Then
  `git switch main && git branch -D flake-update && mac/apply --pull`, and
  update macOS by hand. Updating the lock on main and applying it skips CI,
  and the next `--pull` fails once main's lock moves.
- idb does not move with the flake update: `mac/idb/release.nix` pins a
  release by hand. To move it, take the version (the file's `tag` without
  its `v`) and the two sha256s from `idb-release.json` in Meta's
  `facebook/homebrew-fb` tap, convert each with
  `nix hash convert --hash-algo sha256 <hex>`, and apply it in a PR like the
  Xcode chore.
- simslim does not move with the flake update: `mac/simslim/release.nix` pins
  a release by version and the sha256 of its `macos-arm64` tarball. An open
  pull request upstream ports simslim from Go to Swift, so a release after
  that may need `mac/simslim/package.nix` changed too. Watch issue #73, the
  slim simulator's extra memory on macOS 27.2 hosts.
- Periphery does not move with the flake update either. Cut moves it, by
  `PERIPHERY_VERSION` and `PERIPHERY_SHA256` in its
  `.github/workflows/ios-build.yaml`, and the heartbeat notes when Cut's
  main pins another release than the one on PATH. Since
  cut-watch/cut#1046, Cut's workflow keeps the image's `~/periphery` only
  when its version and its `.sha256` marker match the branch's pin, and
  otherwise replaces it with a warning. So while the image and a branch
  disagree, each of that branch's jobs downloads its own Periphery. A
  branch from before that change still fails on a mismatch: rebase it onto
  Cut's main. To move it:
  1. Merge Cut's change.
  2. Copy both values into `mac/periphery/release.nix` in a PR here, merge
     it, run `mac/apply --pull`, then `build-image` in `worker-shell`.
- Tart, Softnet and idb's client and its Python dependencies also come from
  nixpkgs-unstable, so they move with the same flake update. A new Softnet
  changes the CI slots' plists, whose PATH names its store path, so the apply
  drains and restarts them; a new idb restarts nothing.
- Claude Code moves with the same flake update. It comes from
  nixpkgs-unstable, because 26.05's is too old for Remote Control. The
  apply that brings a new version rewrites worker's server
  plists, which restarts the servers. Their environments and sessions come
  back, but a running session's clean worktree or session folder may not,
  and a session in the middle of a turn loses that turn (see [Claude Remote
  Control](#claude-remote-control)). It does not wait for
  them: unlike the CI slots, they have no drain.
  Their plists change only with Claude Code's store path (a new version,
  or a rebuild of the `procps` or ripgrep it wraps), a server's flags or
  capacity, the git settings in `mac/claude/default.nix`, or the
  environment other modules add (`homelab.worker.environment`, for now
  `DOCKER_HOST`).
  `mac/claude/default.nix` overrides the package's `procps` to keep its Nix
  `top` off sessions' PATH (see "Slim simulators"). If a new version stops
  taking `procps`, the eval fails; drop the override only once the wrapper
  no longer puts `top` on PATH.
- [`check-mac.yml`](ci.md) evaluates the config on every change to `mac/`, so a
  typo fails in CI rather than on the Mac. It cannot build the config: that
  needs a Mac.
