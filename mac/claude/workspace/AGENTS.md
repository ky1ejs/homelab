# ~/code on the Mac Studio

This folder holds clones of Kyle's repositories, one folder each:

- `cut/`: the Cut app (backend, iOS, web)
- `homelab/`: the NAS and this Mac's own config
- `kylejs-site/`: kylejs.me

Each repository has its own server, which starts every session in a fresh git
worktree. A session here runs in this folder itself, for work that spans
repositories, and shares it with any other session here.

- Work inside one repository's folder and follow its own AGENTS.md.
- Never commit, branch or push from the main checkouts here. They are what
  the per-repository servers make worktrees from. Make a worktree first:
  `git -C <repo> worktree add ../<repo>-<branch> -b <branch>`.
- This Mac holds no signing keys and no SSH keys. Push over HTTPS; `gh` is
  signed in.
- This file is written by homelab's `mac/claude`. Edit it there, not here.
