# Sessions on the Mac Studio's any-repo server

Each session here starts in its own empty folder under `~/code/any/sessions/`.
Nothing is checked out yet.

1. Clone the repository the task is about into this folder:
   `gh repo clone OWNER/REPO`. A fresh clone is on the latest default branch.
   If the task names no repository, ask which one.
2. Work inside the clone, and follow its own `AGENTS.md` or `CLAUDE.md`.
3. Commit on a new branch and push it before the session ends. When a
   session ends, its folder is deleted if every clone in it is clean and
   pushed; otherwise the folder is kept. A kept folder untouched for 14 days
   has its unsaved work saved to `~/code/salvage` and is then deleted.

How this differs from a session on a repo's own server (`mac-studio <name>`):

- The clone's instruction files, rules and skills load once you read a file
  inside it. Its agent settings (hooks, permission rules, plugins, `env`) and
  its MCP servers do not: they load only for a session that starts in the
  repo's own folder. If the task needs them, say so, and suggest running it
  on that repo's own server.
- Keep everything inside a clone. A file left in the session's folder itself
  keeps the folder on disk.
- GitHub access is worker's: `cut-watch` repos read and write; `ky1ejs` repos
  read only, unless picked for the write token; never write access to
  `ky1ejs/homelab` or `ky1ejs/kylejs-macOS-bootstrap`.

Services (databases, caches, object storage):

- Run them only as containers, from the repo's own Docker Compose file or
  scripts. `docker` and `docker compose` reach one Linux VM that every session
  on this Mac shares.
- Never install Postgres, Redis, an S3 server or another database on the
  host, by any route: Nix, Homebrew, a downloaded binary or a language
  package. One on the host takes the port for every session, at a version no
  repo chose. If Docker does not answer, stop and say so.
- Remove only containers, volumes and databases you made. Never run
  `docker system prune`, `docker volume prune` or `docker compose down -v` on
  a project another session may be using.
- A container can bind-mount only files under `~/code`.

Tools:

- Use the tools already on `PATH`, which come from this Mac's Nix config, the
  repo's own dependencies (`node_modules`, `bunx`, `npx`) and what the repo's
  own setup scripts install. Never install a tool for every session: no
  `nix profile install`, `npm install -g`, `bun add -g`, `pnpm add -g`,
  `pip install --user`, `cargo install`, `go install`, version managers,
  downloaded binaries in `~/.local/bin`, edits to shell startup files or new
  LaunchAgents. If the task needs a tool that is not here, say so; it gets
  added to the Mac's config for every session.
