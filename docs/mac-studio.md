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
| Access | Tailscale node `mac-studio.tail3df177.ts.net`, tagged `tag:mac-studio`. SSH over the tailnet is Tailscale SSH; SSH on the LAN takes keys only. Remote Management (Screen Sharing or Apple Remote Desktop) logs in with the account password |
| Unlock | `ssh ms-unlock` after any restart but `fdesetup authrestart`. It logs `worker` in. See [FileVault](#filevault) |
| Remote Claude | `worker`'s Remote Control server for Cut, `mac-studio cut`. See [Claude Remote Control](#claude-remote-control) |
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
| Auto-login | Off, because macOS turns it off when FileVault is on. The FileVault unlock logs `worker` in instead |
| An Apple Account | Signed in by hand, for Find My. nix-darwin cannot sign in, nor turn off the keychain sync that [The network](#the-network) needs off |
| Remote Management | macOS only turns it on in System Settings, and runs it or Screen Sharing, not both. It logs in with the account password, not a key |
| The screen lock's delay | Since macOS 13 it needs the admin password or an MDM profile, so `mac/bootstrap` sets it. The config adds the daemon that relies on it |
| Creating the `worker` user | FileVault takes only a user created with an admin password. By hand, in [Claude Remote Control](#claude-remote-control) |
| CI runners, Colima, models | Phases 2, 4 and 5 |
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
phases 2 and 3 need a logged-in session. Unlocking over SSH gives one anyway:
it logs in the user who unlocks. Unlock as `worker`, whose session runs the
[Claude Remote Control](#claude-remote-control) servers. Unlocked as `kylejs`,
the Studio comes up with no servers.

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
user and password as well: give `worker`'s, so `worker` is the one logged in
after the restart. Not yet run on the Studio.

Any other restart stops at the unlock, power cuts included. To test the
unlock, restart with `ssh -t ms sudo shutdown -r now`.

### Unlocking

From the laptop, at home or away:

```sh
ssh ms-unlock
```

It asks for `worker`'s password, prints *System successfully unlocked*, and
closes. A minute or two later the Studio is back on the tailnet with `worker`
logged in: `ssh ms 'stat -f %Su /dev/console'` prints `worker`.

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
while connected and turn back on, unlocked, at the disconnect. `ssh ms-unlock`
needs nothing: its login comes up at a locked screen.

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
   screen should be locked, as it was before the daemon.
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
`claude remote-control` server for each repo with an iOS app, a heartbeat
and two commands for reaching worker's session over SSH. Cut is the only such
repo; homelab and kylejs-site have none, so they get no server.

| Server, as the Claude app lists it | Folder | Each session gets | At once |
|---|---|---|---|
| `mac-studio cut` | `~worker/code/cut` | its own git worktree | 2 |

The server is a launchd agent in worker's login session, started at login
and restarted within 30 seconds whenever it exits. A restart ends the
sessions it was serving; start a new one from the app. It runs only while worker
is logged in, which after a restart means `worker` did the
[unlock](#unlocking).

**There is no Bash sandbox.** It blocks CoreSimulator, which Cut's builds
and tests need. The boundary is worker itself: a standard user, with no SSH or
signing keys, and GitHub tokens that never reach this repo or
kylejs-macOS-bootstrap.

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
write access may ever cover this repo or kylejs-macOS-bootstrap: `mac/apply --pull` runs this repo's main as root and
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
3. **Restart and unlock as worker:** `ssh -t ms sudo shutdown -r now`, then
   `ssh ms-unlock` with worker's password. `ssh ms 'stat -f %Su /dev/console'`
   prints `worker`. The server starts and exits: no repo, no sign-in yet.
   worker's first login opens macOS's new-user Setup Assistant on the
   Studio's own screen; finish it there once, skipping the Apple Account.
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
   mkdir -p ~/code && cd ~/code && gh repo clone cut-watch/cut
   ```

   The token is a file, not a `gh auth login`, so the server's sessions read
   it without the keychain. The first `git config` line drops macOS's
   keychain helper for GitHub. GitHub emails before the token expires; renew
   it by writing the new one over the file.
6. **Sign in to claude.ai**, in the same shell: `claude auth login`. Open
   the link it prints on any device and paste the code back.
7. **Trust Cut's folder and accept Remote Control**, in `worker-shell`. It
   asks `Trust <folder>?` and `Enable Remote Control?` once each; answer
   `y`, wait for the session URL, then Ctrl-C:

   ```sh
   cd ~/code/cut && claude remote-control --spawn same-dir --no-create-session-in-dir
   launchctl kickstart -k "gui/$(id -u)/dev.kylejs.claude-rc.cut"
   ```

   The second line restarts the server, which had exited untrusted.
8. **Point the heartbeat at Healthchecks.io.** Make a check with a 5-minute
   period and 10 minutes' grace, then in `worker-shell` paste its ping URL
   after the first line and press Ctrl-D:

   ```sh
   mkdir -p ~/.config/claude-rc && (umask 077 && cat > ~/.config/claude-rc/ping-url)
   ```

   The URL stays on the Studio, outside the repo.
9. **Done when** the Claude app lists `mac-studio cut`, a session there
   answers, the screensaver from a hot corner asks for worker's password,
   and after a restart and `ssh ms-unlock` it comes back with nothing else
   run.

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
4. If the repo has an iOS app and needs its own server, add it to `servers`
   in [`mac/claude/default.nix`](../mac/claude/default.nix) and clone it to
   `~worker/code/<name>`.

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

### The heartbeat

Every 5 minutes, as worker, [`mac/claude/heartbeat`](../mac/claude/heartbeat)
checks that each server's launchd job is running and that no server log has
a sign-in or trust error since the last run. It kills any xcodebuild of
worker's older than 2 hours and reports swap over 8 GB. All well, it pings
the check; otherwise it pings `/fail` with the problems. Healthchecks.io
emails on `/fail`, and on silence: the Studio off or locked, or `kylejs` did
the unlock.

It never runs `claude auth status`, and nothing else that touches the
sign-in. It keeps each log to its newest 2,000 lines once it passes 10 MB.

Logs, as `kylejs`:

```sh
sudo tail -n 50 ~worker/Library/Logs/claude-rc-cut.log        # one server
sudo tail -n 20 ~worker/Library/Logs/claude-rc-heartbeat.log  # "ok", or the problems
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
- a Cut session pushes a branch over HTTPS and runs a simulator test.

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
- Claude Code moves with the same flake update. It comes from
  nixpkgs-unstable, because 26.05's is too old for Remote Control; nothing
  else does. The apply that brings a new version rewrites worker's server
  plists, which restarts the servers and ends their sessions.
- [`check-mac.yml`](ci.md) evaluates the config on every change to `mac/`, so a
  typo fails in CI rather than on the Mac. It cannot build the config: that
  needs a Mac.
