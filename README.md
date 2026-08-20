# kite

A bird-eye view of every git repo in a directory, and one command to bring each
one's default branch up to date without disturbing what you're doing.

```
$ kite
REPO        BRANCH              DIRTY  ↑↓  vs MAIN  STASH  LAST  PR     CI
caddy       main                -      -   -        -      3h
etcd        main                6      -   -        -      6d
grafana     feat/retry-backoff  -      ↑2  -4       -      19h   #4211  ✓
prometheus  main                -      -   -        1      2d
terraform   fix/1234-nil-deref  2      ↑1  -31      -      23h   #887   ✗
traefik     release/v3          -      ·   -        -      3w
vault       chore/bump-deps     -      -   -8       2      4d    #152   ○

7 repos · 2 dirty · 3 stashes · oldest fetch 1d ago (kite update)
```

Reading that: `etcd` has six uncommitted files. `grafana` is two commits ahead of
its own remote and four behind main, with a green PR. `terraform` is 31 behind
main and its PR is red. `traefik` has no upstream at all. `vault` has two
forgotten stashes and a PR still running.

## Usage

```
kite [filter]            status table
kite status [filter]     same, explicit
kite update [filter]     fetch, fast-forward every default branch, then the table
```

```
--root <dir>             directory holding the repos (default: current directory)
--no-pr                  skip the GitHub PR and CI lookups
-h, --help               usage
```

`kite` lists the repos found directly inside a directory, one level down, so run
it from the folder your checkouts live in or point `--root` at it. `NO_COLOR`
disables color, as does piping the output anywhere.

`filter` matches a repo name or a current branch name, case-insensitively:

```
$ kite grafana                  # one repo
$ kite retry-backoff            # every repo whose branch mentions it
$ kite update release           # update only the repos on a release branch
```

Matching branches as well as names is broader than it looks. A filter of `docs`
also matches a repo sitting on a `docs/rewrite-intro` branch. That's intended,
and harmless, because `update` cannot damage a repo it touches.

## What update does, and what it refuses to do

```
$ kite update
✓  caddy       main +4
·  etcd        up to date
✓  grafana     main +12 (on feat/retry-backoff)
!  prometheus  main diverged from origin/main, skipped
✗  traefik     fetch failed: could not read from remote repository (on release/v3)
```

On the default branch it runs `git merge --ff-only origin/main`. Anywhere else it
runs `git fetch . origin/main:main`, which advances the local `main` ref without a
checkout, so your working tree and current branch don't move even with
uncommitted changes. Git enforces the fast-forward, so a diverged local `main` is
reported and skipped rather than rewritten.

It never forces, stashes, switches branches, or rebases. No code path can lose
uncommitted work.

## Columns

| Column | Meaning |
| --- | --- |
| `DIRTY` | modified plus untracked files |
| `↑↓` | commits ahead of and behind the branch's own upstream, `·` when it has none |
| `vs MAIN` | commits this branch is behind `origin/main` as of the last fetch |
| `STASH` | stashes sitting in the repo |
| `LAST` | age of the newest commit |
| `PR` / `CI` | open PR and its rolled-up check status, for branches that aren't the default |

`PR` and `CI` appear only when there is something to put in them. Without `gh`
installed or authenticated, or with `--no-pr`, or with nothing checked out but
default branches, the two columns are left out entirely rather than shown empty.

`status` never hits the network for git, so `↑↓` and `vs MAIN` are only as fresh
as your last fetch. The footer says how stale that is. `update` refreshes it.

## Install

```
go install .
```

Standard library only, no third-party dependencies. `git` is required; kite exits
with install instructions if it isn't on your PATH. `gh` is optional and only
powers the two PR columns.
