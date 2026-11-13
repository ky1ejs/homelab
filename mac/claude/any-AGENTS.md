# Sessions on the Mac Studio's any-repo server

Each session here starts in its own empty folder under `~/code/any/sessions/`.
Nothing is checked out yet.

1. Clone the repository the task is about into this folder:
   `gh repo clone OWNER/REPO`. A fresh clone is on the latest default branch.
   If the task names no repository, ask which one.
2. Work inside the clone, and follow its own `AGENTS.md` or `CLAUDE.md`.
3. Commit on a new branch and push it before the session ends. When a
   session ends, its folder is deleted if every clone in it is clean and
   pushed; otherwise the folder is kept.

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
