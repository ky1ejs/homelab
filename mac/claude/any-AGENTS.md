# Sessions on the Mac Studio's any-repo server

Each session here starts in its own empty folder under `~/code/any/sessions/`.
Nothing is checked out yet.

The one exception is the server's standing session, titled "mac-studio any
repo folder", which works in `~/code/any` itself. It exists to keep the
server's environment across restarts. If your working directory is
`~/code/any` itself, clone nothing and change nothing here: the sweep never
looks at this folder, and nothing deletes what is left in it. Say that the
task needs a new session from the Claude app, on this environment.

1. Clone the repository the task is about into this folder:
   `gh repo clone OWNER/REPO`. A fresh clone is on the latest default branch.
   If the task names no repository, ask which one.
2. Work inside the clone, and follow its own `AGENTS.md` or `CLAUDE.md`.
3. Commit on a new branch and push it before the session ends. When a
   session ends, its folder is deleted if every clone in it is clean and
   pushed; otherwise the folder is kept. A kept folder that is clean and
   pushed is deleted once nothing has used it for 4 days. A kept folder
   untouched for 2 hours loses its git-ignored DerivedData folders, which a
   build makes again.
   Stop any server you start before the session ends: one left running
   keeps the folder from being swept. One
   untouched for 14 days has its unsaved work saved to `~/code/salvage` and
   is then deleted.

How this differs from a session on a repo's own server (the environment named after the repo, such as `cut`):

- The clone's instruction files, rules and skills load once you read a file
  inside it. Its agent settings (hooks, permission rules, plugins, `env`) and
  its MCP servers do not: they load only for a session that starts in the
  repo's own folder. If the task needs them, say so, and suggest running it
  on that repo's own server.
- Keep everything inside a clone. A file left in the session's folder itself
  keeps the folder on disk.
- GitHub access is worker's: `cut-watch` repos read and write; `ky1ejs` repos
  read only, unless picked for the write token; never write access to
  `ky1ejs/homelab` or `ky1ejs/kylejs-macOS-bootstrap`. Run `gh` from inside
  the clone, or pass `-R OWNER/REPO`, so it picks that repo's token.
  `~/.claude/CLAUDE.md`, "GitHub", has the rest.

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
- To drive an iOS simulator (tap, type, read the screen), use `idb`, which is
  on `PATH` with its companion.
- `ffmpeg` and `ffprobe` are on `PATH`, to turn a screen recording into
  still frames. `~/.claude/CLAUDE.md`, "Screen recordings", has the commands.
- `periphery`, the Swift dead code scanner, is on `PATH` at the release Cut's
  CI uses.
- `app-store` signs and uploads apps to TestFlight with this Mac's App Store
  Connect key. `~/.claude/CLAUDE.md`, "Shipping apps", says how and what
  never to do with the key.
