# AGENTS.md

Instructions for agents working in this repository.

## Every code change must carry its documentation with it

Before finishing any change to code or configuration, **check what documentation
that change has just made wrong** and update it in the same commit. Stale docs
here are not cosmetic: this repo's prose is load-bearing operational knowledge,
and a runbook that no longer matches the scripts is discovered during a restore.

Treat this as part of the change, not a follow-up. A change is not complete while
a document still describes the old behaviour.

### Look in both places

**Markdown files** — the obvious half:

| File | Carries |
|---|---|
| `README.md` (root) | What the homelab is, what runs where |
| `obsidian-vault/README.md` | Operating runbook — numbered setup steps, restore procedure, service table |
| `obsidian-vault/ARCHITECTURE.md` | Mermaid diagrams, component tables, data flow |
| `obsidian-vault/DECISIONS.md` | *Why* each choice was made, including rejected alternatives |
| `obsidian-vault/.env.example` | Every tunable, with the reasoning for its default |
| `obsidian-vault/research-CLAUDE.md` | The research agent's standing instructions, installed into the scratch volume |
| `vault-mcp/README.md` | MCP server behaviour, auth flow, tool surface |
| `dashboard/README.md` | Dashboard design, its trust boundary, and what it deliberately cannot do |

**Comments in code** — the half that gets missed. In this repo, script and Go
header comments carry the rationale, not just the mechanics:

- `obsidian-vault/scripts/*.sh` — header blocks explain why the approach was
  chosen over the alternative, and inline comments guard specific traps.
  `scratch-sweep.sh` is the only script here that deletes anything, so its
  guards are documented individually rather than as a group
- `obsidian-vault/vault-{claude,research}-settings.json` — the `_comment` array
  at the top of each is the real documentation for that agent's tool policy, and
  the two are deliberately asymmetric: one denies the web and reads the vault,
  the other has the web and cannot see the vault. A change that makes them look
  more alike is almost certainly wrong in one of them
- `vault-mcp/*.go`, `dashboard/*.go` — same convention. In `dashboard/` the
  tests are part of the documentation too: `agent_test.go` states, in executable
  form, exactly what the socket-holding process will refuse
- `bin/homelab` — the usage block at the top is parsed by `usage()` with
  `sed -n '3,21p'`, so adding a command means editing that block AND moving
  the range, which is easy to forget and silently truncates `--help`
- `obsidian-vault/docker-compose.yml`, `Dockerfile` — comments justify mounts,
  pins, and the absence of published ports
- `dashboard/docker-compose.yml` — justifies the two mounts that are exceptions
  to this repo's usual posture: the Docker socket, and the checkout bound at its
  own host path

### What to check for

1. **Contradiction.** Does any document now state something the code no longer
   does? Diagrams and tables date fastest — they encode counts, names and
   retention numbers that a one-line change can falsify.
2. **Invalidated rationale.** A comment saying "X because Y" is wrong when Y
   stops being true, even if X is still what the code does. This is the most
   damaging kind of stale doc, because it reads as verified reasoning. If a
   change undercuts a recorded decision, correct the record rather than leaving
   two claims standing.
3. **Defaults.** Changing a default means updating `.env.example` *and* any
   prose quoting the old value.
4. **New tunables.** A new environment variable is undocumented until it appears
   in `.env.example` with its reasoning, not just its name.
5. **Reversals.** When a change undoes an earlier decision, say so in
   `DECISIONS.md` with the date and what changed the answer. Do not silently
   delete the old rationale — the rejected alternatives are the point.

### Scope

Update what the change actually touched. This is not licence for a documentation
sweep: unrelated staleness found along the way should be mentioned, not fixed
silently inside someone else's change.
