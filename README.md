# kite

A bird-eye view of every git repo in the workspace, and one command to bring
each one's default branch up to date without disturbing what you're doing.

```
$ kite
REPO                       BRANCH                              DIRTY  ↑↓  vs MAIN  STASH  LAST  PR     CI
cloudscanner               main                                -      -   -        1      1h
customer-asset-collector   UP-5731-coverage-last-PR            -      -   -4       1      19h   #1876  ✓
mongodb-service            UP-5731-coverage                    -      -   -        -      18h   #151   ✓
rdb-service                main                                6      -   -        -      6d
upwind-azure-serverless    fix/preflight-group-inherited-rbac  -      -   -        -      22h

15 repos · 4 dirty · 3 stashes · oldest fetch 1d ago (kite update)
```

## Usage

```
kite [filter]            status table
kite status [filter]     same, explicit
kite update [filter]     fetch, fast-forward every default branch, then the table
```

`filter` matches a repo name or a current branch name, case-insensitively, so
`kite UP-5731` shows only the repos carrying that ticket.

Flags: `--no-pr` skips the GitHub lookups. `NO_COLOR` disables color.

The root directory is `$KITE_ROOT`, or `~/Apps` when unset.

## What update does, and what it refuses to do

On the default branch it runs `git merge --ff-only origin/main`. Anywhere else
it runs `git fetch . origin/main:main`, which advances the local `main` ref
without a checkout, so your working tree and current branch don't move even with
uncommitted changes. Git enforces the fast-forward; a diverged local `main` is
reported and skipped.

It never forces, stashes, switches branches, or rebases. No code path can lose
uncommitted work.

## Columns

| Column | Meaning |
| --- | --- |
| `DIRTY` | modified plus untracked files |
| `↑↓` | commits ahead of and behind the branch's own upstream, `·` when there is none |
| `vs MAIN` | commits this branch is behind `origin/main` as of the last fetch |
| `LAST` | age of the newest commit |
| `PR` / `CI` | open PR and its rolled-up check status, for branches that aren't the default |

`status` never hits the network for git, so `↑↓` and `vs MAIN` are only as fresh
as your last fetch. The footer says how stale that is. `update` is what refreshes it.

## Install

```
go install .
```

Standard library only. Requires `git`, and `gh` for the PR columns.
