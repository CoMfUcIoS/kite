package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ponytail: 8 concurrent repos and one subprocess per datum. 15 repos comes to
// roughly a hundred short-lived git processes, measured at 0.5s wall clock.
// Process spawn dominates, so if that ever grates, `status --porcelain=v2
// --branch` returns branch, ahead, behind and dirty entries in a single call.
const maxParallel = 8

const prTimeout = 3 * time.Second

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

type PR struct {
	Number int
	CI     string
}

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

	PR  *PR
	Err error
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

func collectAll(paths []string) []Repo {
	out := make([]Repo, len(paths))
	fan(len(paths), func(i int) { out[i] = collect(paths[i]) })
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

func collect(path string) Repo {
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

// --- GitHub ---

type ghCheck struct {
	Status     string `json:"status"`     // CheckRun
	Conclusion string `json:"conclusion"` // CheckRun
	State      string `json:"state"`      // StatusContext
}

type ghPR struct {
	Number            int       `json:"number"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
}

// attachPRs looks up the open PR for every repo sitting on a non-default
// branch, and reports whether gh was even available. Anything that goes wrong
// leaves the columns blank; a missing gh, a stale token or a slow API must
// never cost you the rest of the table.
func attachPRs(repos []Repo) (ghFound bool) {
	if _, err := exec.LookPath("gh"); err != nil {
		return false
	}
	fan(len(repos), func(i int) {
		r := &repos[i]
		if r.Err != nil || r.Detached || r.Default == "" || r.Branch == r.Default {
			return
		}
		r.PR = fetchPR(r.Path, r.Branch)
	})
	return true
}

func fetchPR(dir, branch string) *PR {
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--head", branch, "--state", "open", "--limit", "1",
		"--json", "number,statusCheckRollup")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var list []ghPR
	if json.Unmarshal(out, &list) != nil || len(list) == 0 {
		return nil
	}
	return &PR{Number: list[0].Number, CI: rollup(list[0].StatusCheckRollup)}
}

// rollup collapses every check on a PR into one verdict: any failure wins,
// then any pending, otherwise it passed.
func rollup(checks []ghCheck) string {
	if len(checks) == 0 {
		return ""
	}
	pending := false
	for _, c := range checks {
		switch verdict(c) {
		case ciFail:
			return ciFail
		case ciPending:
			pending = true
		}
	}
	if pending {
		return ciPending
	}
	return ciPass
}

// verdict normalises one check. A CheckRun reports status plus conclusion, a
// StatusContext reports state, and gh returns both shapes in the same array.
func verdict(c ghCheck) string {
	s := strings.ToUpper(c.Conclusion)
	if s == "" {
		s = strings.ToUpper(c.State)
	}
	switch s {
	case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return ciFail
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return ciPass
	}
	return ciPending
}
