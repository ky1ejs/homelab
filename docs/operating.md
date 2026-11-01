# Operating it

[`bin/homelab`](../bin/homelab) is the entry point when you are SSH'd into the NAS.
It wraps the per-stack scripts rather than replacing them — `deploy` delegates to
a stack's own `scripts/deploy.sh` where one exists, so `--sync-only` and the
re-pair warning keep working.

```sh
bin/homelab stacks                 # the stacks this CLI can act on
bin/homelab status                 # every stack: containers, images, snapshot and voice freshness
bin/homelab deploy vault-mcp       # pull, verify provenance, recreate
bin/homelab env check vault-mcp    # .env mode and any key left blank
bin/homelab token rotate           # new MCP_TOKEN, recreate, print the header to paste into Claude
bin/homelab setup monitoring       # configure the monitoring hub from the repo; re-runnable, and the restore
bin/homelab url                    # the connector URL the Funnel is actually serving
bin/homelab url gmail-mcp          # the same, for the Gmail connector
bin/homelab attach                 # the vault-claude tmux session
bin/homelab attach research        # the vault-research tmux session
bin/homelab install                # put bin/ on PATH, so it is just `homelab` from anywhere

bin/homelab update                 # git pull this checkout
bin/homelab git status             # git against this checkout
bin/homelab snapshots log --author='Claude Voice'   # the vault's own history
```

**Every command takes an explicit stack, and there is no `--all`** — the [shared
contract](shared-contract.md) says never restart another stack's containers, and the cheapest
way to honour that is to make it unexpressible.

## Running it from any directory

Run this once, from the checkout:

```sh
bin/homelab install
```

It appends one guarded line to your login profile that adds this checkout's
`bin/` to the **end** of `PATH`, then tells you which file it wrote. Open a new
SSH session (or `.` the file it names) and `homelab status` works from anywhere,
with no `cd`. A second run changes nothing.

- **Which profile.** `~/.profile`, which is what the QTS admin shell (busybox
  `ash`) reads. If your shell is bash and `~/.bash_profile` or `~/.bash_login`
  exists, it writes that instead, because bash then never reads `~/.profile`.
  zsh and fish are refused with the path to add yourself.
- **Why `PATH` and not a symlink in `/usr/local/bin`.** The QTS root filesystem
  is a RAM disk rebuilt at boot, so a link there is gone after a restart. The
  script also resolves its own symlinks now, so a link from somewhere that *does*
  persist works too.
- **Whether it survives a restart has not been checked on this host.** If the
  profile resolves outside `/share`, `install` warns that it may be on the RAM
  disk. If `homelab` is missing after the next restart, the recovery is the same
  command by its full path:

  ```sh
  /share/CE_CACHEDEV4_DATA/homelab/bin/homelab install
  ```

  Once a restart has shown which way it goes, record the answer in
  [`host.md`](host.md) next to the other `PATH` trap.
- **Interactive logins only.** `ssh nas homelab status` still fails, for the same
  reason `ssh nas docker ps` does: a non-interactive session reads no profile.
  See [`host.md`](host.md).

## Getting the repo onto the NAS without host git

Container Station gives QTS a `docker` binary but no `git`, and **installing one
via Entware is a bad trade**: Entware is a well-documented way to break the SSH
`PATH`, and the `PATH` is how you reach `docker` in the first place.

The CLI handles this itself — `homelab git`, `homelab update` and
`homelab snapshots` use host git when it exists and a pinned `alpine/git`
container when it does not. **So the NAS never needs git installed.**

The one thing the CLI cannot do is the *initial* clone, because it lives inside
the repo it would be cloning. That one stays a one-liner (the repo is public, so
no credential is involved):

```sh
cd /share/CE_CACHEDEV4_DATA
docker run --rm -v "$PWD:/git" -w /git -u "$(id -u):$(id -g)" -e HOME=/tmp \
    alpine/git clone https://github.com/ky1ejs/homelab.git
```

`-u` matters: without it the clone lands owned by root and the stack's own files
end up unreadable to the account everything else runs as. After that,
`bin/homelab update` does the pulling.

**Nothing else needs host git either.** Every snapshot commit happens inside a
container whose image already has it, and `homelab status` and
`homelab snapshots` borrow git from any running container that mounts
`/snapshots` when the host has none.

Output is ASCII-only and colour is dropped when stdout is not a terminal, so it
stays readable on a QTS session with no UTF-8 locale and pastes cleanly into a
chat window. Both properties are asserted in CI.
