# kite

A bird-eye view of every git repo in a directory, and one command to bring each
one's default branch up to date without disturbing what you're doing.

```
$ kite
REPO        BRANCH                  DIRTY  ↑↓  vs MAIN  STASH  LAST  PR     RV  CI
caddy       main                    -      -   -        -      3h
etcd        feat/lease-ttl-tuning   6      ↑3  -        1      4d    #204       ○
grafana     feat/retry-backoff      -      ↑2  -4       -      19h   #4211  ✓   ✓
prometheus  main                    -      -   -        -      6d
            ↳ feat/remote-write-v2  -      -   -14!     -      3h    #591   ✗   ✗ lint +2
            ↳ feat/queue-metrics    -      ·            -      1d    #602   ·   ○
terraform   fix/module-nil-deref    -      ↑1  -31      -      23h   #887   ·   ○
traefik     release/v3              -      ·   -        -      3w
vault       chore/bump-deps         -      -   -8       -      4d    #152   ✎

7 repos · 1 dirty · 1 stash · 6 open PRs · 1 red · oldest fetch 1d ago (kite update) · took 1.4s · git 0.31s · GitHub 1.1s

waiting on you
  caddy    #150  4d  feat: add h3 fallback probe
  grafana  #98   2d  chore: rotate registry secrets cache
```

Reading that: `etcd` has six uncommitted files and a PR still waiting on CI.
`grafana`'s PR is approved and green. `prometheus` itself sits on `main`, but it
has two PRs open on branches you're not standing on: `feat/remote-write-v2`, 14
commits behind main with changes requested and lint failing plus two more
checks, and `feat/queue-metrics`, whose branch kite can't find at all, so `vs
MAIN` is blank instead of a number. `terraform` is 31 behind main and waiting
on a reviewer. `traefik` has no upstream at all. `vault`'s PR is still a draft.
And two of those PRs, on `caddy` and `grafana`, are waiting on a review from
you.

While it works, kite shows a spinner with a count (`reading repos 12/45`, then
`asking GitHub 3/27`) on stderr, and clears it before the table prints. It only
appears on a terminal, so pipes, scripts and `--json` consumers never see it.
The footer ends with how long the run took, split into local git work and
GitHub lookups.

On a terminal narrower than the table, kite shortens the REPO and BRANCH
columns with a `…` in the middle, keeping each name's start and end, until the
rows fit. It stops at 16 characters per column. Piped output and `--json` always
carry the full names, and `COLUMNS` overrides the detected width.

When the output is taller than the terminal, kite pages it the way git does:
through `KITE_PAGER`, else `PAGER`, else `less`. A bare `less` runs as
`less -FRX`, so output that fits one screen prints and exits as before, colour
survives, and the table stays on screen after you quit. Pass `--no-pager`, or
set `KITE_PAGER` to `cat` or an empty string, to turn it off. Pipes and `--json`
are never paged.

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
--ignored                prune only: also remove worktrees whose only leftovers are ignored files
--no-pager               print straight to the terminal, skipping the pager
--json                   one JSON document on stdout instead of the table
--version                print the version
-h, --help               usage
```

`kite` lists the repos found directly inside a directory, one level down, plus
every linked worktree of those repos wherever it lives. Run it from the folder
your checkouts live in or point `--root` at it. `NO_COLOR`
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

Where `main` is checked out, in this repo or any of its worktrees, it runs
`git merge --ff-only origin/main` there. When nothing has `main` checked out it
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
prometheus  release/v3          checked out in prometheus         skipped

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
of the work. So there are four tiers:

| What kite found                              | What it does                                       |
| -------------------------------------------- | -------------------------------------------------- |
| an ancestor of the default branch            | deletes with `git branch -d`, so git double-checks |
| upstream gone, and `gh` confirms a merged PR | deletes with `-D`                                  |
| upstream kept, a merged PR of yours contains the branch tip | deletes with `-D` |
| upstream gone, no merged PR found            | reports it, needs `--force`                        |

The third row covers squash merges that kept their head branch, which leave no
local trace at all. kite asks GitHub once per repo for your 100 most recent
merged PRs, and a branch with new commits beyond its PR's head is left alone.

A branch checked out in any worktree of the repo is never deleted, not even with
`--force`, because removing it would mean switching away and kite never switches
branches.

There's one exception, for the worktree-per-PR workflow. Once a PR merges, the
branch's linked worktree is dead weight. If that
worktree is clean, with nothing modified, untracked or ignored, prune offers to
remove it (`would remove worktree and delete`, with the directory's path in the
reason) and `--delete` runs `git worktree remove` before deleting the branch. The
footer counts those removals on their own, so you can see how many directories
go before you say yes. The same tiers apply, so an
unconfirmed merge still needs `--force`. The main checkout is never removed, and
neither is a worktree without a merged PR or a deleted remote branch to show its
work is done.

A finished worktree that still holds files is skipped, and the reason names
them (`PR #12 merged, worktree app-done holds cover.out`). When every one of them
is ignored, such as build output or a coverage file, `--ignored` lets prune
remove it anyway, and the reason lists the ignored files that go with it. Check
that list for a local `.env` before adding `--delete`. Modified or untracked
files always keep the worktree.

## Finding forgotten work

```
$ kite stash
REPO     STASH      AGE             SUBJECT
caddy    stash@{0}  7 days ago      WIP on main: fix(1201): drop the retry ceiling
grafana  stash@{0}  32 minutes ago  WIP on feat/retry-backoff: half-finished jitter
grafana  stash@{1}  2 days ago      WIP on feat/retry-backoff: first attempt, superseded

3 stashes · git -C <repo> stash show -p <ref>
```

The status table gives you a stash _count_. A count cannot tell you whether that
is twenty minutes of work or a week-old dead end. This can.

## Linked worktrees

Worktrees of one repo share its branches, stashes and PRs, so kite reports those
once. Each worktree still gets its own row under the main checkout, drawn as a
branch of it, with its own branch, dirty count and PR:

```
REPO                         BRANCH                DIRTY  ↑↓  vs MAIN  STASH  LAST
grafana                      main                  -      -   -        1      2h
├ _worktrees/grafana-alerts  feat/alert-routing    3      ↑1  -4                1h
└ _worktrees/grafana-otel    fix/otel-exporter     -      ·   -4                5d
```

The name is the worktree's path relative to `--root`, or starts with `~/` when it
lives outside the root altogether. A blank `STASH` on a worktree row isn't zero:
the count sits on the main checkout's row. `stash` and `prune` list each stash
and branch once, and `update` fetches each repo once.

A worktree kept outside the root, say in a `_worktrees` folder, still shows up.
kite asks `git worktree list` for every repo that has linked worktrees, and a
worktree in the root pulls in its main checkout the same way. A worktree whose
directory was deleted is skipped.

## Jumping between repos

```
$ cd $(kite path grafana)
```

A process cannot change its parent's directory, so there is no `kite cd`. `path`
prints the directory instead and lets the shell do the moving. It matches the
names of directories in the root only, which means it needs no git calls at all
and returns instantly. Worktrees kept elsewhere aren't found by `path`.

If the filter matches more than one repo it fails rather than printing a list,
because handing `cd` two arguments produces an error that explains nothing:

```
$ kite path agent
kite: 2 repos match "agent", be more specific:
        grafana-agent
        prometheus-agent
```

## Machine-readable output

Scripts and agents shouldn't scrape a layout meant for people. `--json` works
on `status`, `update`, `stash` and `prune`, and prints one JSON document with no
colour and no footer:

```
$ kite --no-pr --json grafana
{
  "repos": [
    {
      "name": "grafana",
      "path": "/home/you/src/grafana",
      "where": "grafana",
      "branch": "feat/retry-backoff",
      "default": "main",
      "detached": false,
      "dirty": 2,
      "ahead": 2,
      "behind": 0,
      "noUpstream": false,
      "behindMain": 4,
      "conflicts": false,
      "rewritten": false,
      "stashes": 1,
      "lastCommit": "2026-03-02T09:14:05Z",
      "fetchedAt": "2026-03-02T08:40:11Z"
    }
  ],
  "reviewQueue": [],
  "prLookupFailed": 0,
  "ghMissing": false,
  "timing": { "totalMs": 1412, "gitMs": 312, "githubMs": 1100 }
}
```

Rows come in the same order as the table. A row can also carry `pr` (`number`,
`review`, `ci`, `failing`, `extraFailing`), `otherPRs` for the indented sub-rows,
and `error` when kite couldn't read the repo. A worktree row carries
`worktreeOf`, its main checkout's name, and no `stashes` key, because the count
lives on the main checkout. Times are RFC 3339 and left out when unknown.

`update --json` prints the same document after updating, plus an `updates`
array of `{repo, branch, default, status, delta, error}`. `stash --json` is an
array of `{repo, ref, age, createdAt, subject}`, and `prune --json` an array of
`{repo, branch, verdict, reason, action, worktree}`. `worktree` is the directory
`--delete` would remove, and it's there only when there is one. `action` is
`would delete`, `would remove worktree and delete`, `deleted`, `needs --force`,
`skipped` or `failed`. `path` rejects `--json`, since it already prints nothing
but a path.

## Columns

`kite --help` ends with the same legend in short form, plus every value `--json`
can emit, so you don't need this page open to read the table.

| Column      | Meaning                                                                                                                                                                                                                                                                  |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `DIRTY`     | modified plus untracked files                                                                                                                                                                                                                                            |
| `↑↓`        | commits ahead of and behind the branch's own upstream; `·` when there's nothing to compare against — no upstream configured, the upstream is `[gone]`, or (on a sub-row) the branch never resolved to anything; `rewritten` when the upstream was force-pushed and the local branch is just its old version, so pushing would undo the rewrite                                                           |
| `vs MAIN`   | commits this branch is behind `origin/main` as of the last fetch; a trailing `!` means merging `main` in would conflict. Blank, not `-`, when kite can't resolve the branch to a ref or doesn't know the default branch — `-` means level with main, blank means unknown |
| `STASH`     | stashes sitting in the repo                                                                                                                                                                                                                                              |
| `LAST`      | age of the newest commit                                                                                                                                                                                                                                                 |
| `PR` / `CI` | open PR and its rolled-up check status, for branches that aren't the default                                                                                                                                                                                             |
| `RV`        | review state: `✓` approved, `✗` changes requested, `·` waiting on reviewers, `✎` draft                                                                                                                                                                                   |

`PR`, `RV` and `CI` appear only when there is something to put in them. Without
`gh` installed or authenticated, or with `--no-pr`, or with nothing checked out
but default branches, the columns are left out entirely rather than shown empty.
`RV` is also blank on a row with a PR but no reviewers assigned yet.

`status` never hits the network for git, so `↑↓` and `vs MAIN` are only as fresh
as your last fetch. The footer says how stale that is. `update` refreshes it.

kite turns on git's untracked cache for its own `status` calls, which makes a
large repo's `DIRTY` count several times faster to work out. The cache trusts
directory mtimes, so on a filesystem that doesn't keep those up to date, such as
some network mounts, a new untracked file can go uncounted until its directory
changes again.

## Branches you aren't standing on

A repo can sit on `main` and still have your PR open on a branch you switched
away from, or never cloned at all. Those appear as indented rows under their
repo:

```
REPO        BRANCH                  DIRTY  ↑↓  vs MAIN  STASH  LAST  PR     RV  CI
prometheus  main                    -      -   -        -      6d
            ↳ feat/remote-write-v2  -      -   -14!     -      3h    #591   ✗   ✗ lint +2
            ↳ feat/queue-metrics    -      ·            -      1d    #602   ·   ○
```

`DIRTY` and `STASH` show a dash on those rows, not a count. Both belong to the
working tree and the repo, not to a branch nobody has checked out.

`feat/queue-metrics` is the "never cloned at all" case: kite can't find that
branch locally or on `origin`, so it has nothing to compare against main, or
even against its own upstream. `vs MAIN` renders blank rather than `-`, and
`↑↓` renders `·` rather than `-`, because both would otherwise misread as
"level" instead of "unknown".

The `!` on `-14!` means merging `main` into that branch would conflict. kite
works this out with `git merge-tree`, which writes only to the object store: it
never checks out, merges, stashes or moves anything.

**It is exact for `git merge main` and approximate for `git rebase main`.** A
rebase replays your commits one at a time and can conflict on an intermediate
step even when the final trees merge cleanly. Treat it as a hint that saves you
a doomed attempt, not a guarantee.

## What's waiting on you

Below the table, kite lists open PRs where you're a requested reviewer, oldest
first by when they were opened, narrowed to repos in this directory. The age
next to each is how long ago it was created, not when it last saw activity, so
a three-week-old PR with a five-minute-old comment still sorts and reads as
three weeks old:

```
waiting on you
  caddy    #150  4d  feat: add h3 fallback probe
  grafana  #98   2d  chore: rotate registry secrets cache
```

Drafts are left out. So is anything in a repo whose directory name doesn't
match its GitHub name. `--no-pr` skips this along with the PR columns.

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

| Commit prefix                           | Effect                                  |
| --------------------------------------- | --------------------------------------- |
| `fix:`                                  | patch bump, listed under Bug Fixes      |
| `feat:`                                 | minor bump, listed under Features       |
| `feat!:` or a `BREAKING CHANGE:` footer | minor bump while below 1.0, major after |
| `chore:`, `docs:`, `refactor:`, `test:` | no release, no changelog entry          |

On every push to `main` the workflow opens or updates a single release PR titled
`chore(main): release kite X.Y.Z`, carrying the version bump and the generated
`CHANGELOG.md`. Merging that PR is what creates the git tag and the GitHub
release. Nothing is tagged until you merge it.

The first release is pinned to `0.1.0` via `initial-version`; without it
release-please starts at `1.0.0`.
