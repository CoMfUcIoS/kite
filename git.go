package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ponytail: one subprocess per datum. 23 repos comes to roughly two hundred
// short-lived git processes, measured at 0.65s wall clock. Process spawn
// dominates, so if that ever grates, `status --porcelain=v2 --branch` returns
// branch, ahead, behind and dirty entries in a single call.
//
// 32 rather than 8 because the gh lookups are network-bound and now run for
// every repo: at 8 the 23 calls took three waves instead of one.
const maxParallel = 32

// CI verdicts.
const (
	ciPass    = "pass"
	ciFail    = "fail"
	ciPending = "pending"
)

// Update outcomes.
const (
	statusAdvanced = "advanced"
	statusCreated  = "created"
	statusCurrent  = "current"
	statusDiverged = "diverged"
	statusBlocked  = "blocked"
	statusError    = "error"
)

type Repo struct {
	Name string
	Path string

	Branch   string // short SHA when detached
	Default  string // resolved default branch, empty when origin has none
	Detached bool

	Dirty      int
	Ahead      int
	Behind     int
	BehindMain int
	Stashes    int
	NoUpstream bool

	LastCommit time.Time
	FetchedAt  time.Time

	PR     *PR
	Others []PRRow // open PRs on branches this repo does not have checked out
	PRErr  bool    // the gh lookup for this repo failed; not the same as "no open PRs"
	Err    error

	// Refs holds every local and origin branch, so sub-rows and the filter
	// need no further subprocesses.
	Refs map[string]refMeta
	// LocalBranches lists local branches except the default, for the filter.
	// origin/* is deliberately absent: every repo has an origin/main, so
	// including remotes would make `kite main` match every repo.
	LocalBranches []string

	// Conflicts means merging origin/<default> into the current branch would
	// conflict. Local data, so it survives --no-pr.
	Conflicts bool

	// Rewritten means the branch diverges from its upstream only because the
	// upstream was force-pushed: the local tip is an old upstream value.
	Rewritten bool

	// Linked worktrees share one git dir, and with it branches, stashes and
	// PRs. CommonDir is that dir, empty when git can't say. Only a group's
	// leader reads repo-wide data, and a zero-value Repo is its own leader.
	CommonDir  string
	Linked     bool // .git is a file: a linked worktree, not the main checkout
	Follower   bool
	LeaderName string
	// Where is Path relative to --root, or ~-abbreviated when outside it.
	Where string
}

// locate fills Where, so a row shows where a worktree kept outside the root
// actually lives.
func locate(repos []Repo, root string) {
	rr := realPath(root)
	home, _ := os.UserHomeDir()
	for i := range repos {
		p := realPath(repos[i].Path)
		if rel, err := filepath.Rel(rr, p); err == nil && !strings.HasPrefix(rel, "..") {
			repos[i].Where = rel
		} else if rel, err := filepath.Rel(realPath(home), p); home != "" && err == nil && !strings.HasPrefix(rel, "..") {
			repos[i].Where = "~/" + rel
		} else {
			repos[i].Where = p
		}
	}
}

func groupKey(r Repo) string {
	if r.CommonDir == "" {
		return r.Path
	}
	return r.CommonDir
}

// markLeaders picks one leader per shared git dir: the main checkout, or the
// first worktree by name when a filter dropped the main checkout. Run it after
// filtering, so a filtered-out main checkout never leaves a group leaderless.
func markLeaders(repos []Repo) {
	lead := map[string]int{}
	for i, r := range repos {
		if j, seen := lead[groupKey(r)]; !seen || outranks(r, repos[j]) {
			lead[groupKey(r)] = i
		}
	}
	for i := range repos {
		l := lead[groupKey(repos[i])]
		repos[i].Follower = i != l
		repos[i].LeaderName = repos[l].Name
	}
}

func outranks(a, b Repo) bool {
	if a.Linked != b.Linked {
		return !a.Linked
	}
	return a.Name < b.Name
}

type Result struct {
	Repo    string
	Branch  string
	Default string
	Status  string
	Delta   int
	Err     error
}

// git runs one git command in dir. exec bypasses the shell, so the `git`->`hub`
// alias in the user's zsh config cannot interfere here.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// discover returns every direct child of root that holds a .git entry. It does
// not recurse: the workspace keeps all repos at one level.
func discover(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		// .git is a directory in a normal clone and a file in a linked worktree.
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// withWorktrees appends every linked worktree of the discovered repos, wherever
// it lives, along with the main checkout of a worktree found in the root.
// Discovered paths keep their spelling and order.
func withWorktrees(paths []string) []string {
	listed := make([][]string, len(paths))
	fan(len(paths), func(i int) {
		if hasWorktrees(paths[i]) {
			listed[i] = worktreeList(paths[i])
		}
	})
	seen := map[string]bool{}
	for _, p := range paths {
		seen[realPath(p)] = true
	}
	out := paths
	for _, ws := range listed {
		for _, w := range ws {
			if !seen[realPath(w)] {
				seen[realPath(w)] = true
				out = append(out, w)
			}
		}
	}
	return out
}

// hasWorktrees is a stat, not a git call, so repos without linked worktrees
// cost nothing. A .git file means path is itself a linked worktree.
func hasWorktrees(path string) bool {
	fi, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return true
	}
	_, err = os.Stat(filepath.Join(path, ".git", "worktrees"))
	return err == nil
}

// worktreeList skips bare entries and prunable ones, whose directory is gone.
func worktreeList(path string) []string {
	out, err := git(path, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var dirs []string
	for _, block := range strings.Split(out, "\n\n") {
		dir, skip := "", false
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				dir = strings.TrimPrefix(line, "worktree ")
			case line == "bare", strings.HasPrefix(line, "prunable"):
				skip = true
			}
		}
		if dir != "" && !skip {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// realPath is the dedupe key: git reports worktrees absolute and with
// symlinks resolved, while discovered paths are spelled however --root was.
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// withConflicts controls whether collect spends a merge-tree subprocess per
// repo on the conflict marker. Only status and update render it; prune and
// stash never do, so it would be pure waste there.
func collectAll(paths []string, withConflicts bool) []Repo {
	out := make([]Repo, len(paths))
	fan(len(paths), func(i int) {
		out[i] = collect(paths[i], withConflicts)
		prog.tick()
	})
	return out
}

// fan runs fn for each index 0..n, at most maxParallel at a time.
func fan(n int, fn func(int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}

func collect(path string, withConflicts bool) Repo {
	r := Repo{Name: filepath.Base(path), Path: path}

	branch, err := git(path, "branch", "--show-current")
	if err != nil {
		r.Err = err
		return r
	}
	if branch == "" {
		// Detached HEAD, or a fresh repo with no commits yet.
		r.Detached = true
		if sha, err := git(path, "rev-parse", "--short", "HEAD"); err == nil {
			r.Branch = sha
		} else {
			r.Branch = "no commits"
		}
	} else {
		r.Branch = branch
	}

	r.Default = defaultBranch(path)

	if s, err := git(path, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		r.CommonDir = s
	}
	if fi, err := os.Stat(filepath.Join(path, ".git")); err == nil && !fi.IsDir() {
		r.Linked = true
	}

	// The untracked cache lives in the index and spares status a full walk
	// for untracked files, which dominated its cost. It trusts directory
	// mtimes, as git does wherever the cache is enabled.
	if s, err := git(path, "-c", "core.untrackedCache=true", "status", "--porcelain"); err == nil {
		r.Dirty = countLines(s)
	}

	// Output order is "<behind>\t<ahead>". A missing upstream is not an error
	// worth surfacing, it just means nothing to compare against.
	if s, err := git(path, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		fmt.Sscan(s, &r.Behind, &r.Ahead)
		if r.Ahead > 0 && r.Behind > 0 {
			r.Rewritten = upstreamRewritten(path)
		}
	} else {
		r.NoUpstream = true
	}

	if r.Default != "" && r.Branch != r.Default {
		if s, err := git(path, "rev-list", "--count", "HEAD..origin/"+r.Default); err == nil {
			r.BehindMain, _ = strconv.Atoi(s)
		}
		if withConflicts && r.BehindMain > 0 {
			r.Conflicts = conflicts(path, "HEAD", r.Default)
		}
	}

	if s, err := git(path, "stash", "list"); err == nil {
		r.Stashes = countLines(s)
	}

	if s, err := git(path, "log", "-1", "--format=%ct"); err == nil {
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
			r.LastCommit = time.Unix(secs, 0)
		}
	}

	if fi, err := os.Stat(filepath.Join(gitDir(path), "FETCH_HEAD")); err == nil {
		r.FetchedAt = fi.ModTime()
	}

	r.Refs = refs(path)
	for name := range r.Refs {
		// %(refname:short) renders refs/remotes/origin/HEAD as the bare
		// "origin", with no slash, so the origin/ prefix check below misses
		// it and it would otherwise land in LocalBranches.
		if name == "origin" || strings.HasPrefix(name, "origin/") || name == r.Default {
			continue
		}
		r.LocalBranches = append(r.LocalBranches, name)
	}
	sort.Strings(r.LocalBranches)

	return r
}

// upstreamRewritten reports whether HEAD is reachable from an earlier value of
// its upstream. The reflog message is no help: a force-push made from another
// worktree logs as a plain "update by push".
func upstreamRewritten(path string) bool {
	old, err := git(path, "rev-list", "-g", "--max-count=100", "@{upstream}")
	if err != nil || old == "" {
		return false
	}
	args := append([]string{"rev-list", "--count", "HEAD", "--not"}, strings.Fields(old)...)
	n, err := git(path, args...)
	return err == nil && n == "0"
}

func gitDir(path string) string {
	d := filepath.Join(path, ".git")
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	// Linked worktree or unusual layout: ask git where the real git dir is.
	if resolved, err := git(path, "rev-parse", "--absolute-git-dir"); err == nil {
		return resolved
	}
	return d
}

// defaultBranch prefers origin/HEAD, then falls back to probing for the usual
// names. The fallback is not hypothetical: some clones never get origin/HEAD set.
func defaultBranch(path string) string {
	if s, err := git(path, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(s, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if _, err := git(path, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+b); err == nil {
			return b
		}
	}
	return ""
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// --- update ---

// updateAll updates the leaders only: worktrees share their refs, and
// concurrent fetches into one git dir fail on ref locks.
func updateAll(repos []Repo) []Result {
	var leaders []Repo
	for _, r := range repos {
		if !r.Follower {
			leaders = append(leaders, r)
		}
	}
	out := make([]Result, len(leaders))
	fan(len(leaders), func(i int) {
		out[i] = update(leaders[i])
		prog.tick()
	})
	return out
}

// update fast-forwards the repo's default branch. It never checks out, never
// stashes, never rebases and never forces, so uncommitted work is untouchable.
func update(r Repo) Result {
	res := Result{Repo: r.Name, Branch: r.Branch, Default: r.Default, Status: statusError}
	if r.Err != nil {
		res.Err = r.Err
		return res
	}

	if _, err := git(r.Path, "fetch", "--prune", "--quiet", "origin"); err != nil {
		res.Err = err
		return res
	}

	// Re-resolve: the fetch may have just created origin/HEAD or origin/main.
	def := defaultBranch(r.Path)
	if def == "" {
		res.Err = errors.New("origin has no main or master branch")
		return res
	}
	res.Default = def

	before, _ := git(r.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+def)

	var err error
	if wt := checkedOutAt(r.Path, def); wt != "" {
		// Checked out in some worktree, maybe this one, where fetch below
		// would refuse. A no-network ff-only merge there refuses rather than
		// overwriting a modified file.
		_, err = git(wt, "merge", "--ff-only", "origin/"+def)
	} else {
		// Not checked out anywhere. Fetching from "." moves the local ref without a checkout,
		// so the working tree and current branch stay exactly where they are.
		// Git enforces fast-forward and exits non-zero otherwise.
		_, err = git(r.Path, "fetch", ".", "origin/"+def+":"+def)
	}
	if err != nil {
		res.Status, res.Err = classify(err), err
		return res
	}

	after, _ := git(r.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+def)
	switch {
	case before == "":
		res.Status = statusCreated
	case before == after:
		res.Status = statusCurrent
	default:
		res.Status = statusAdvanced
		if n, err := git(r.Path, "rev-list", "--count", before+".."+after); err == nil {
			res.Delta, _ = strconv.Atoi(n)
		}
	}
	return res
}

// checkedOutAt returns the worktree that has branch checked out, or "".
func checkedOutAt(path, branch string) string {
	s, _ := git(path, "for-each-ref", "--format=%(worktreepath)", "refs/heads/"+branch)
	return s
}

func classify(err error) string {
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "non-fast-forward"), strings.Contains(m, "not possible to fast-forward"):
		return statusDiverged
	case strings.Contains(m, "would be overwritten"), strings.Contains(m, "local changes"):
		return statusBlocked
	}
	return statusError
}

// --- prune ---

// Branch is a local branch that looks finished: either already merged into the
// default branch, or its remote counterpart is gone.
type Branch struct {
	Repo    string
	Path    string
	Name    string
	Default string

	Merged   bool // an ancestor of origin/<default>
	Gone     bool // upstream branch deleted
	PRNumber int  // a merged PR for this branch, 0 when none or unknown
	Current  bool // checked out in some worktree, so it cannot be deleted without switching

	CheckedOutIn string // directory name of the worktree holding it
	// Worktree is a clean linked worktree holding the branch, which deleting
	// the branch removes first. Set instead of Current.
	Worktree string
	// holder is that worktree before there's proof the work finished.
	holder string

	Err error
}

// Prune verdicts.
const (
	pruneMerged  = "merged"
	prunePR      = "pr-merged"
	pruneUnsure  = "unverified"
	pruneCurrent = "current"
	pruneErr     = "error"
)

func (b Branch) verdict() string {
	switch {
	case b.Err != nil:
		return pruneErr
	case b.Current:
		// Deleting the checked-out branch means switching away from it, and
		// kite never switches branches.
		return pruneCurrent
	case b.Merged:
		return pruneMerged
	case b.PRNumber > 0:
		return prunePR
	}
	return pruneUnsure
}

// deletable answers whether kite is willing to delete this branch. An upstream
// that merely vanished is not proof of a merge: closing a pull request without
// merging also deletes its branch, and that branch still holds the only copy of
// the work. Those need force.
func (b Branch) deletable(force bool) bool {
	switch b.verdict() {
	case pruneMerged, prunePR:
		return true
	case pruneUnsure:
		return force
	}
	return false
}

func staleBranchesAll(repos []Repo, useGH bool) []Branch {
	perRepo := make([][]Branch, len(repos))
	live := make([][]Branch, len(repos))
	fan(len(repos), func(i int) {
		if !repos[i].Follower {
			perRepo[i], live[i] = scanBranches(repos[i])
			prog.tick()
		}
	})

	var all []Branch
	for _, bs := range perRepo {
		all = append(all, bs...)
	}

	// Only the ambiguous ones are worth a network call.
	if useGH && ghAvailable() {
		fan(len(all), func(i int) {
			b := &all[i]
			if b.Gone && !b.Merged && !b.Current {
				b.PRNumber = mergedPR(b.Path, b.Name)
			}
		})
		// A squash merge that kept its head branch leaves no local trace,
		// so ask once per repo which of its live branches already merged.
		finished := make([][]Branch, len(repos))
		fan(len(repos), func(i int) {
			if len(live[i]) > 0 {
				finished[i] = matchMerged(live[i], listMergedPRs(repos[i].Path), repos[i].Path)
			}
		})
		for _, bs := range finished {
			all = append(all, bs...)
		}
	}
	return all
}

// matchMerged keeps the live branches a merged PR accounts for: same name, and
// the local tip is the PR's head or behind it. New commits on a reused branch
// name fail the ancestry check, and so does a head this clone never fetched.
func matchMerged(live []Branch, prs []ghMergedPR, path string) []Branch {
	var out []Branch
	for _, b := range live {
		for _, p := range prs {
			if p.HeadRefName != b.Name {
				continue
			}
			if _, err := git(path, "merge-base", "--is-ancestor", b.Name, p.HeadRefOid); err != nil {
				continue
			}
			b.PRNumber = p.Number
			if b.holder != "" {
				b.Worktree, b.Current = b.holder, false
			}
			out = append(out, b)
			break
		}
	}
	return out
}

func staleBranches(r Repo) []Branch {
	stale, _ := scanBranches(r)
	return stale
}

// scanBranches fetches with --prune first, because the "[gone]" marker that
// identifies a squash-merged branch only appears once the deleted remote branch
// has been pruned locally. live holds pushed branches that are neither merged
// by ancestry nor gone: only a merged PR can say those are finished.
func scanBranches(r Repo) (stale, live []Branch) {
	fail := func(err error) ([]Branch, []Branch) {
		return []Branch{{Repo: r.Name, Path: r.Path, Err: err}}, nil
	}
	if r.Err != nil {
		return fail(r.Err)
	}
	if _, err := git(r.Path, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return fail(err)
	}
	def := defaultBranch(r.Path)
	if def == "" {
		return nil, nil
	}

	out, err := git(r.Path, "for-each-ref",
		"--format=%(refname:short)%00%(upstream:track)%00%(worktreepath)%00%(upstream)", "refs/heads")
	if err != nil {
		return fail(err)
	}

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) < 4 || fields[0] == "" || fields[0] == def {
			continue
		}
		b := Branch{
			Repo:    r.Name,
			Path:    r.Path,
			Name:    fields[0],
			Default: def,
			Gone:    fields[1] == "[gone]",
		}
		// %(worktreepath) is set when any worktree of the repo has the
		// branch checked out, not only this one.
		if wt := fields[2]; wt != "" {
			b.CheckedOutIn = filepath.Base(wt)
			if removableWorktree(wt, r.Path) {
				b.holder = wt
			}
			// Gone, not just merged: a worktree freshly branched from main
			// also reads as merged, before any work has started in it.
			if b.Gone && b.holder != "" {
				b.Worktree = wt
			} else {
				b.Current = true
			}
		}
		// A squash merge leaves no ancestry, which is exactly why the Gone
		// check above carries most of the weight.
		if _, err := git(r.Path, "merge-base", "--is-ancestor", b.Name, "origin/"+def); err == nil {
			b.Merged = true
		}
		switch {
		case b.Merged || b.Gone:
			stale = append(stale, b)
		case fields[3] != "":
			live = append(live, b)
		}
	}
	return stale, live
}

// removableWorktree is a linked worktree other than the one kite runs git
// from, with nothing uncommitted and nothing ignored: git worktree remove
// deletes ignored files, a local .env included. The main checkout never
// qualifies.
func removableWorktree(wt, from string) bool {
	if realPath(wt) == realPath(from) {
		return false
	}
	if fi, err := os.Stat(filepath.Join(wt, ".git")); err != nil || fi.IsDir() {
		return false
	}
	s, err := git(wt, "status", "--porcelain", "--ignored")
	return err == nil && s == ""
}

func deleteBranch(b Branch) error {
	if b.Worktree != "" {
		// No --force: git refuses a tree that got dirty since it was listed.
		if _, err := git(b.Path, "worktree", "remove", b.Worktree); err != nil {
			return err
		}
	}
	// Ancestry-merged branches go through -d so git double-checks us. The rest
	// need -D, because a squash merge leaves nothing for -d to verify.
	flag := "-D"
	if b.Merged {
		flag = "-d"
	}
	_, err := git(b.Path, "branch", flag, b.Name)
	return err
}

// --- stashes ---

type Stash struct {
	Repo    string
	Ref     string
	Age     string
	Subject string
}

func stashesAll(repos []Repo) []Stash {
	perRepo := make([][]Stash, len(repos))
	fan(len(repos), func(i int) {
		perRepo[i] = stashList(repos[i])
		if !repos[i].Follower {
			prog.tick()
		}
	})

	var all []Stash
	for _, ss := range perRepo {
		all = append(all, ss...)
	}
	return all
}

func stashList(r Repo) []Stash {
	if r.Err != nil || r.Follower {
		return nil
	}
	// NUL separators: a stash subject is a commit message and can contain
	// anything printable, including whatever delimiter looked safe.
	out, err := git(r.Path, "stash", "list", "--format=%gd%x00%cr%x00%s")
	if err != nil || out == "" {
		return nil
	}

	var stashes []Stash
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) < 3 {
			continue
		}
		stashes = append(stashes, Stash{
			Repo: r.Name, Ref: fields[0], Age: fields[1], Subject: fields[2],
		})
	}
	return stashes
}

// parseTrack reads git's "[ahead 1, behind 4]" upstream:track format, or the
// "[gone]" marker left once a deleted remote branch has been pruned. An empty
// string means either no upstream or level with it; callers distinguish those
// two using %(upstream:short), which is empty only in the first case. gone can
// be true alongside a non-empty %(upstream:short): git keeps the configured
// upstream name even after the remote-tracking ref is pruned, so callers must
// check gone too rather than trusting upstream-name emptiness alone.
func parseTrack(s string) (ahead, behind int, gone bool) {
	s = strings.Trim(s, "[]")
	switch s {
	case "":
		return 0, 0, false
	case "gone":
		return 0, 0, true
	}
	for _, part := range strings.Split(s, ", ") {
		var n int
		if _, err := fmt.Sscanf(part, "ahead %d", &n); err == nil {
			ahead = n
			continue
		}
		if _, err := fmt.Sscanf(part, "behind %d", &n); err == nil {
			behind = n
		}
	}
	return ahead, behind, false
}

type refMeta struct {
	Upstream   string
	Ahead      int
	Behind     int
	Gone       bool
	LastCommit time.Time
}

// refs reads every local and origin branch in one subprocess. Sub-rows need
// per-branch data, and one call per branch would cost more than the feature.
func refs(path string) map[string]refMeta {
	out, err := git(path, "for-each-ref",
		"--format=%(refname:short)%00%(upstream:short)%00%(upstream:track)%00%(committerdate:unix)",
		"refs/heads", "refs/remotes/origin")
	if err != nil || out == "" {
		return nil
	}
	m := make(map[string]refMeta)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		ahead, behind, gone := parseTrack(f[2])
		meta := refMeta{Upstream: f[1], Ahead: ahead, Behind: behind, Gone: gone}
		if secs, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			meta.LastCommit = time.Unix(secs, 0)
		}
		m[f[0]] = meta
	}
	return m
}

// refFor resolves a PR branch to a ref that exists here, preferring the local
// copy. The origin fallback is what covers a PR whose branch you never cloned
// or have since deleted.
func refFor(branch string, m map[string]refMeta) string {
	if _, ok := m[branch]; ok {
		return branch
	}
	if _, ok := m["origin/"+branch]; ok {
		return "origin/" + branch
	}
	return ""
}

// fillRows gives each sub-row the local data its columns need, then orders
// them newest commit first.
func fillRows(path, def string, rows []PRRow, m map[string]refMeta) {
	for i := range rows {
		ref := refFor(rows[i].Branch, m)
		if ref == "" {
			// Nothing at all is known about this branch: NoUpstream stays
			// true so the ↑↓ cell reads "unknown" rather than "level".
			rows[i].NoUpstream = true
			continue
		}
		rows[i].Resolved = true
		meta := m[ref]
		rows[i].Ahead, rows[i].Behind = meta.Ahead, meta.Behind
		// meta.Gone: the remote branch was deleted and pruned, but git keeps
		// the upstream name configured, so upstream-name emptiness alone
		// would miss this and render "level" instead of "unknown".
		rows[i].NoUpstream = meta.Upstream == "" || meta.Gone
		rows[i].LastCommit = meta.LastCommit
		if def == "" {
			// No default branch is known for anyone, so vs MAIN cannot be
			// computed. Un-resolve the row rather than leave BehindMain at
			// its zero value, which would render as "level with main".
			rows[i].Resolved = false
			continue
		}
		if s, err := git(path, "rev-list", "--count", ref+"..origin/"+def); err == nil {
			rows[i].BehindMain, _ = strconv.Atoi(s)
		}
	}
	// Stable: two PRs on the same un-checked-out branch (item 2) share a
	// Branch and LastCommit, so an unstable sort could flip their render
	// order between runs on identical input.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].LastCommit.After(rows[j].LastCommit) })
}

// conflicts reports whether merging origin/<def> into ref would conflict. It
// writes only to the object store: no checkout, no index, no working tree.
//
// Exit 0 is a clean merge and exit 1 is a conflict. Anything else means
// unknown, which includes git older than 2.38 rejecting --write-tree, and
// unknown shows no marker. That is why no version detection is needed.
//
// Exact for `git merge main`. A rebase replays commits one at a time and can
// conflict on an intermediate step even when the final trees merge cleanly,
// so for rebases this is a hint, not a guarantee.
func conflicts(path, ref, def string) bool {
	if ref == "" || def == "" {
		return false
	}
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--name-only", ref, "origin/"+def)
	cmd.Dir = path
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	err := cmd.Run()
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}
