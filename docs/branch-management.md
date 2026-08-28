# Branch management

Porto can switch the branch used by a project, run multiple branches at once, and remove branches that Git can prove are fully merged.

## Switch branches

Each project card has a searchable picker for local and remote-tracking branches. The default branch, `main`, and `master` are pinned first when available.

Selecting a different branch for a running project stops the process, switches the worktree, updates its HTTPS hostname, and restarts it. Porto refuses to switch when the worktree is dirty or the target branch is already checked out elsewhere.

The equivalent CLI command is:

```sh
porto branch <project> <branch>
```

## Run concurrent branch instances

Use **New instance** to run another branch without disturbing the original checkout. Porto creates a `worktrees` directory under its platform-specific user configuration directory, or `$PORTO_HOME/worktrees` when `PORTO_HOME` is set. It then runs the detected dependency setup so ignored artifacts such as `node_modules` are available.

Each instance receives an independent process, port, logs, and controls. The dashboard groups these runtimes under their source project.

The default branch keeps the base hostname. Other branches use compact labels: `copilot/improve-elemental-resistances-system` becomes `cop-imp-ele-res-sys`, so a project named `2dnd` receives `https://2dnd-cop-imp-ele-res-sys.porto.localhost/`. Porto shortens long labels and adds a deterministic suffix when needed to keep hostnames valid and unique.

Managed instances can be removed from their project card after their worktree is clean. Porto stops the process, removes the Git worktree, and deletes only that instance's runtime metadata and logs.

## Clean up merged branches

Open the dashboard's **Branch hygiene** panel to enable automatic local or remote cleanup. Porto checks every 10 seconds and removes only branches whose complete Git history is already reachable from the repository's default branch.

The current branch, default branch, unmerged branches, and configured protected names or glob patterns are never removed.

Remote cleanup is disabled by default and requires confirmation because it permanently deletes branches from the primary Git remote. Optional pruning runs `git fetch --prune` with interactive credential prompts disabled. Squash-merged and rebase-merged branches are left alone unless Git can prove their complete history is merged.
