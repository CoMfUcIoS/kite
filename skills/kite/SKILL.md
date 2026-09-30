---
name: kite
description: Use when working across many git repos or worktrees that sit side by side in one directory, and you need to know which have uncommitted work, stashes, branches behind or conflicting with main, open PRs with review or CI state, or finished branches to delete; or to fast-forward every default branch, or to find one repo's path.
---

# kite

## Overview

kite reads every git repo one level inside a directory, plus each one's linked worktrees
wherever they live, and answers multi-repo questions in one call. Reach for it before writing a `for repo in */` loop. Those loops miss worktrees (their
`.git` is a file, not a directory), can't tell a conflicting branch from a behind one, and
`git fetch` alone never moves a local `main`.

Run it from the directory that holds the repos, or pass `--root <dir>`.

## Quick reference

| Question                                               | Command                     |
| ------------------------------------------------------ | --------------------------- |
| What's dirty, stashed, behind, conflicting, in review? | `kite` (or `kite <filter>`) |
| Bring every default branch up to date                  | `kite update`               |
| Which stashes exist, and how old?                      | `kite stash`                |
| Which branches are finished?                           | `kite prune` (dry run)      |
| cd into a repo                                         | `cd "$(kite path <name>)"`  |
| Local state only, no GitHub                            | add `--no-pr`               |

`--no-pr` also removes the `PR`, `RV` and `CI` columns and the indented PR rows. Leave it off
for any question about PRs, reviews or CI.

A filter matches a repo name **or** its current branch, case-insensitively. `path` matches the
names of directories in the root only, so it can't find a worktree kept elsewhere, and it fails
rather than guessing when more than one repo matches.

## Reading the table

- `DIRTY` is modified plus untracked files. `STASH` is the stash count.
- `↑↓` is ahead/behind the branch's own upstream. `·` means there's nothing to compare against.
- `vs MAIN` is how far behind `origin/<default>` the branch is. A trailing `!` means merging main
  would conflict. `-` means level with main. Blank means unknown.
- `PR`, `RV` and `CI` show only when there's data. RV: `✓` approved, `✗` changes requested,
  `·` waiting, `✎` draft.
- Indented `└` rows are your open-PR branches that aren't checked out.
- Linked worktrees sit under their main checkout as `├`/`└` rows in the REPO column, named by
  their path relative to the root (`~/…` when outside it). Stashes, PR sub-rows and the
  `stash`, `prune` and `update` lines come once per repo, so a blank STASH on a worktree row
  isn't zero.
- The footer says how stale the last fetch is. `status` never fetches, so run `kite update` first
  when freshness matters.

## Safety

- `kite update` only fast-forwards. It never forces, stashes, switches branches or rebases, so
  it's safe with uncommitted work. A diverged `main` is reported and skipped.
- `kite prune` changes nothing without `--delete`. `--force` also deletes branches whose upstream
  is gone with no merged PR found, which may be the only copy of that work.
- "Clean up my branches" means: run `kite prune`, show the user the dry-run list, and ask. Run
  `--delete` or `--force` only after they say yes to that list.

## Common mistakes

- Looping `git fetch` and calling the default branches updated. Use `kite update`.
- Trusting `vs MAIN` right after a long break. Check the footer's fetch age.
- Running `kite` from inside one repo. It scans the current directory's children, so point
  `--root` at the parent.
- Treating a table as machine output. Colour turns off when piped, but the layout is for people:
  filter it with `kite <filter>` rather than parsing columns.
