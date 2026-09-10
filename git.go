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

// withConflicts controls whether collect spends a merge-tree subprocess per
// repo on the conflict marker. Only status and update render it; prune and
// stash never do, so it would be pure waste there.
func collectAll(paths []string, withConflicts bool) []Repo {
	out := make([]Repo, len(paths))
	fan(len(paths), func(i int) { out[i] = collect(paths[i], withConflicts) })
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

	if s, err := git(path, "status", "--porcelain"); err == nil {
		r.Dirty = countLines(s)
	}

	// Output order is "<behind>\t<ahead>". A missing upstream is not an error
	// worth surfacing, it just means nothing to compare against.
	if s, err := git(path, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		fmt.Sscan(s, &r.Behind, &r.Ahead)
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

func updateAll(repos []Repo) []Result {
	out := make([]Result, len(repos))
	fan(len(repos), func(i int) { out[i] = update(repos[i]) })
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
	if !r.Detached && r.Branch == def {
		// On the default branch. A no-network ff-only merge, which refuses
		// rather than overwriting a modified file.
		_, err = git(r.Path, "merge", "--ff-only", "origin/"+def)
	} else {
		// Elsewhere. Fetching from "." moves the local ref without a checkout,
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
	Current  bool // checked out here, so it cannot be deleted without switching

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
	fan(len(repos), func(i int) { perRepo[i] = staleBranches(repos[i]) })

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
	}
	return all
}

// staleBranches fetches with --prune first, because the "[gone]" marker that
// identifies a squash-merged branch only appears once the deleted remote branch
// has been pruned locally.
func staleBranches(r Repo) []Branch {
	fail := func(err error) []Branch {
		return []Branch{{Repo: r.Name, Path: r.Path, Err: err}}
	}
	if r.Err != nil {
		return fail(r.Err)
	}
	if _, err := git(r.Path, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return fail(err)
	}
	def := defaultBranch(r.Path)
	if def == "" {
		return nil
	}

	out, err := git(r.Path, "for-each-ref",
		"--format=%(refname:short)%00%(upstream:track)%00%(HEAD)", "refs/heads")
	if err != nil {
		return fail(err)
	}

	var branches []Branch
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) < 3 || fields[0] == "" || fields[0] == def {
			continue
		}
		b := Branch{
			Repo:    r.Name,
			Path:    r.Path,
			Name:    fields[0],
			Default: def,
			Gone:    fields[1] == "[gone]",
			Current: strings.TrimSpace(fields[2]) == "*",
		}
		// A squash merge leaves no ancestry, which is exactly why the Gone
		// check above carries most of the weight.
		if _, err := git(r.Path, "merge-base", "--is-ancestor", b.Name, "origin/"+def); err == nil {
			b.Merged = true
		}
		if b.Merged || b.Gone {
			branches = append(branches, b)
		}
	}
	return branches
}

func deleteBranch(b Branch) error {
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
	fan(len(repos), func(i int) { perRepo[i] = stashList(repos[i]) })

	var all []Stash
	for _, ss := range perRepo {
		all = append(all, ss...)
	}
	return all
}

func stashList(r Repo) []Stash {
	if r.Err != nil {
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
