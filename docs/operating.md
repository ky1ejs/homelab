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

bin/homelab update                 # git pull this checkout
bin/homelab git status             # git against this checkout
bin/homelab snapshots log --author='Claude Voice'   # the vault's own history
```

**Every command takes an explicit stack, and there is no `--all`** — the [shared
contract](shared-contract.md) says never restart another stack's containers, and the cheapest
way to honour that is to make it unexpressible.

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
