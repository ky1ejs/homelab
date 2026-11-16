# Sessions on the Mac Studio

Installed by `mac/claude` in the homelab repo, as worker's own `~/.claude/CLAUDE.md`. Every session on this Mac reads it.

## The standing sessions

Each Remote Control server keeps one standing session in its own folder, titled "mac-studio NAME main checkout" or "mac-studio any repo folder". It exists only so the server keeps its environment across restarts.

If your working directory is `~/code/NAME` itself, a repo's main checkout, or `~/code/any` itself, you are one of these. Change nothing there: no edits, no branch switches, no clones, no setup or bootstrap scripts, no builds. The repo sync keeps the main checkout on the latest main, and every other session's worktree branches from it. Say that the task needs a new session started from the Claude app on this environment, which gets a worktree or a folder of its own.

Sessions under `~/code/NAME/.claude/worktrees/` or `~/code/any/sessions/` are not standing sessions: this section does not apply to them.

## Cleaning up what you start

Every session on this Mac runs as the same user, worker, so worker's permissions let you stop any session's servers and simulators. Stop only your own: other sessions are using theirs.

- Name every simulator you create so that it ends in ` @` and your folder's name, the last part of `~/code/NAME/.claude/worktrees/X` or `~/code/any/sessions/X`: `xcrun simctl create "Cut E2E @X" ...`. Use a new name for each one, and refer to it by that literal name. Don't keep its UDID in a file in `/tmp`, which every session shares.
- Start servers from inside your folder, so that their working directory is in it.
- When you are done with them, run `session-cleanup` from your folder. It stops every process working in your folder, apart from your session and its tools, and deletes every simulator named for it. `session-cleanup --dry-run` lists them first. It also runs by itself when your session ends.
- Never stop processes or simulators by a pattern that matches other sessions' too: no `pkill -f`, `killall`, `xcrun simctl delete all` or `xcrun simctl shutdown all`. To stop one process, use its PID: `$!` gives it when you start the process in the background.
- Remove containers, databases and other shared services' data with the repo's own teardown, such as Cut's `./scripts/teardown`, which removes only your worktree's. Don't stop or remove the shared containers themselves.
