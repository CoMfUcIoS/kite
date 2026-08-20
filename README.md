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
kite prune [filter]      list finished local branches; --delete removes them
kite stash [filter]      every stash across every repo, with age and subject
kite path [filter]       print one repo path, for: cd $(kite path api)
```

```
--root <dir>             directory holding the repos (default: current directory)
--no-pr                  skip the GitHub lookups
--delete                 prune only: actually delete the branches
--force                  prune only: also delete branches whose merge is unconfirmed
--version                print the version
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

## Pruning finished branches

```
$ kite prune
caddy       feat/http3-probe    PR #3076 merged                   would delete
caddy       fix/1234-nil-deref  PR #3065 merged                   would delete
etcd        chore/bump-deps     merged into main                  would delete
grafana     spike/new-parser    upstream gone, merge unconfirmed  needs --force
prometheus  release/v3          checked out here                  skipped

3 would delete · 1 needs --force · 1 checked out · nothing changed (kite prune --delete)
```

Dry run by default. `kite prune --delete` does the work.

The reason this is not a one-line `git branch --merged` wrapper: **a squash merge
leaves no ancestry.** The branch's commits never become ancestors of `main`, so
git reports it as unmerged and you keep it forever. The signal that actually
identifies a squash-merged branch is its deleted remote counterpart, which only
appears as `[gone]` after a `git fetch --prune`. In the workspace this was
built against, that distinction accounted for two thirds of the dead branches.

But a vanished upstream is not proof of a merge either: closing a pull request
without merging also deletes its branch, and that branch may hold the only copy
of the work. So there are three tiers:

| What kite found | What it does |
| --- | --- |
| an ancestor of the default branch | deletes with `git branch -d`, so git double-checks |
| upstream gone, and `gh` confirms a merged PR | deletes with `-D` |
| upstream gone, no merged PR found | reports it, needs `--force` |

The checked-out branch is never deleted, not even with `--force`, because
removing it would mean switching away and kite never switches branches.

## Finding forgotten work

```
$ kite stash
REPO     STASH      AGE             SUBJECT
caddy    stash@{0}  7 days ago      WIP on main: fix(1201): drop the retry ceiling
grafana  stash@{0}  32 minutes ago  WIP on feat/retry-backoff: half-finished jitter
grafana  stash@{1}  2 days ago      WIP on feat/retry-backoff: first attempt, superseded

3 stashes · git -C <repo> stash show -p <ref>
```

The status table gives you a stash *count*. A count cannot tell you whether that
is twenty minutes of work or a week-old dead end. This can.

## Jumping between repos

```
$ cd $(kite path grafana)
```

A process cannot change its parent's directory, so there is no `kite cd`. `path`
prints the directory instead and lets the shell do the moving. It matches repo
names only, which means it needs no git calls at all and returns instantly.

If the filter matches more than one repo it fails rather than printing a list,
because handing `cd` two arguments produces an error that explains nothing:

```
$ kite path agent
kite: 2 repos match "agent", be more specific:
        grafana-agent
        prometheus-agent
```

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

```sh
curl -fsSL https://raw.githubusercontent.com/comfucios/kite/main/install.sh | sh
```

That grabs the latest release for your OS and architecture, updates an older copy
in place, and does nothing at all if you already have the newest one. Run it again
any time to update. macOS and Linux, amd64 and arm64.

```sh
# somewhere other than ~/.local/bin
KITE_INSTALL_DIR=/usr/local/bin curl -fsSL https://raw.githubusercontent.com/comfucios/kite/main/install.sh | sh

# pin an exact release
KITE_VERSION=v0.1.0 curl -fsSL https://raw.githubusercontent.com/comfucios/kite/main/install.sh | sh
```

Or with a Go toolchain:

```sh
go install github.com/comfucios/kite@latest    # latest tag
go install .                                   # from a checkout
```

Standard library only, no third-party dependencies. `git` is required; kite exits
with install instructions if it isn't on your PATH. `gh` is optional and only
powers the two PR columns.

`kite --version` reports the release tag for an installed build, or the git
revision for one built from a checkout.

## Releases

Releases are automated with [release-please](https://github.com/googleapis/release-please).
Commit messages on `main` must follow
[Conventional Commits](https://www.conventionalcommits.org/), because that is what
decides the next version:

| Commit prefix | Effect |
| --- | --- |
| `fix:` | patch bump, listed under Bug Fixes |
| `feat:` | minor bump, listed under Features |
| `feat!:` or a `BREAKING CHANGE:` footer | minor bump while below 1.0, major after |
| `chore:`, `docs:`, `refactor:`, `test:` | no release, no changelog entry |

On every push to `main` the workflow opens or updates a single release PR titled
`chore(main): release kite X.Y.Z`, carrying the version bump and the generated
`CHANGELOG.md`. Merging that PR is what creates the git tag and the GitHub
release. Nothing is tagged until you merge it.

The first release is pinned to `0.1.0` via `initial-version`; without it
release-please starts at `1.0.0`.
