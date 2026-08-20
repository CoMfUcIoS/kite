package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func isolateGit(t *testing.T) {
	t.Helper()
	// Keep the tests independent of whatever git config this machine carries.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "kite test")
	t.Setenv("GIT_AUTHOR_EMAIL", "kite@test")
	t.Setenv("GIT_COMMITTER_NAME", "kite test")
	t.Setenv("GIT_COMMITTER_EMAIL", "kite@test")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return out
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// workspace builds a bare origin and a clone holding one commit on main.
// origin/HEAD is deliberately left unset, so every test also exercises the
// defaultBranch fallback that real clones without origin/HEAD depend on.
func workspace(t *testing.T) (origin, clone string) {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()

	origin = filepath.Join(root, "origin.git")
	mustGit(t, root, "init", "-q", "--bare", "-b", "main", origin)

	clone = filepath.Join(root, "work")
	mustGit(t, root, "clone", "-q", origin, clone)
	mustGit(t, clone, "symbolic-ref", "HEAD", "refs/heads/main")
	writeFile(t, clone, "one.txt", "one")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "one")
	mustGit(t, clone, "push", "-q", "-u", "origin", "main")
	return origin, clone
}

// addRemoteCommit advances origin/main from a throwaway second clone.
func addRemoteCommit(t *testing.T, origin, name string) {
	t.Helper()
	tmp := t.TempDir()
	other := filepath.Join(tmp, "other")
	mustGit(t, tmp, "clone", "-q", origin, other)
	writeFile(t, other, name, name)
	mustGit(t, other, "add", ".")
	mustGit(t, other, "commit", "-qm", name)
	mustGit(t, other, "push", "-q", "origin", "main")
}

func TestCollect(t *testing.T) {
	_, clone := workspace(t)
	writeFile(t, clone, "one.txt", "changed") // modified
	writeFile(t, clone, "extra.txt", "extra") // untracked

	r := collect(clone)
	if r.Err != nil {
		t.Fatalf("collect: %v", r.Err)
	}
	if r.Branch != "main" {
		t.Errorf("Branch = %q, want main", r.Branch)
	}
	if r.Default != "main" {
		t.Errorf("Default = %q, want main (fallback probe failed)", r.Default)
	}
	if r.Detached {
		t.Error("Detached = true on a named branch")
	}
	if r.Dirty != 2 {
		t.Errorf("Dirty = %d, want 2", r.Dirty)
	}
	if r.NoUpstream {
		t.Error("NoUpstream = true despite push -u")
	}
	if r.Ahead != 0 || r.Behind != 0 {
		t.Errorf("Ahead/Behind = %d/%d, want 0/0", r.Ahead, r.Behind)
	}
	if r.LastCommit.IsZero() {
		t.Error("LastCommit not populated")
	}
	if r.Name != "work" {
		t.Errorf("Name = %q, want work", r.Name)
	}
}

func TestCollectDetachedHEAD(t *testing.T) {
	_, clone := workspace(t)
	sha := mustGit(t, clone, "rev-parse", "--short", "HEAD")
	mustGit(t, clone, "checkout", "-q", "--detach", "HEAD")

	r := collect(clone)
	if !r.Detached {
		t.Fatalf("Detached = false, branch = %q", r.Branch)
	}
	if r.Branch != sha {
		t.Errorf("Branch = %q, want short sha %q", r.Branch, sha)
	}
}

// The load-bearing case: update must advance local main while leaving the
// checked-out feature branch and every uncommitted byte alone.
func TestUpdateFastForwardsFromDirtyFeatureBranch(t *testing.T) {
	origin, clone := workspace(t)
	mustGit(t, clone, "switch", "-qc", "feature")
	writeFile(t, clone, "feature.txt", "feature")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "feature work")
	writeFile(t, clone, "uncommitted.txt", "PRECIOUS")

	addRemoteCommit(t, origin, "two.txt")
	addRemoteCommit(t, origin, "three.txt")

	mainBefore := mustGit(t, clone, "rev-parse", "main")

	res := update(collect(clone))
	if res.Status != statusAdvanced {
		t.Fatalf("Status = %q, err = %v", res.Status, res.Err)
	}
	if res.Delta != 2 {
		t.Errorf("Delta = %d, want 2", res.Delta)
	}
	if got := mustGit(t, clone, "rev-parse", "main"); got == mainBefore {
		t.Error("local main did not move")
	} else if want := mustGit(t, clone, "rev-parse", "origin/main"); got != want {
		t.Errorf("main = %s, origin/main = %s", got, want)
	}
	if got := mustGit(t, clone, "branch", "--show-current"); got != "feature" {
		t.Errorf("current branch = %q, want feature", got)
	}
	if b, err := os.ReadFile(filepath.Join(clone, "uncommitted.txt")); err != nil || string(b) != "PRECIOUS\n" {
		t.Errorf("uncommitted work lost: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(clone, "feature.txt")); err != nil {
		t.Errorf("feature branch content disturbed: %v", err)
	}
	// main's new files must NOT appear: nothing was checked out.
	if _, err := os.Stat(filepath.Join(clone, "two.txt")); err == nil {
		t.Error("origin/main content leaked into the feature working tree")
	}
}

func TestUpdateReportsDivergenceWithoutMutating(t *testing.T) {
	origin, clone := workspace(t)
	writeFile(t, clone, "local.txt", "local")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "local only, never pushed")
	mustGit(t, clone, "switch", "-qc", "feature")
	writeFile(t, clone, "uncommitted.txt", "PRECIOUS")

	addRemoteCommit(t, origin, "two.txt")

	mainBefore := mustGit(t, clone, "rev-parse", "main")

	res := update(collect(clone))
	if res.Status != statusDiverged {
		t.Fatalf("Status = %q, want %q (err = %v)", res.Status, statusDiverged, res.Err)
	}
	if got := mustGit(t, clone, "rev-parse", "main"); got != mainBefore {
		t.Error("main moved despite divergence")
	}
	if got := mustGit(t, clone, "branch", "--show-current"); got != "feature" {
		t.Errorf("current branch = %q, want feature", got)
	}
	if b, err := os.ReadFile(filepath.Join(clone, "uncommitted.txt")); err != nil || string(b) != "PRECIOUS\n" {
		t.Errorf("uncommitted work lost: %q err=%v", b, err)
	}
}

func TestUpdateOnDefaultBranch(t *testing.T) {
	origin, clone := workspace(t)
	addRemoteCommit(t, origin, "two.txt")

	res := update(collect(clone))
	if res.Status != statusAdvanced || res.Delta != 1 {
		t.Fatalf("Status = %q Delta = %d, want advanced/1 (err = %v)", res.Status, res.Delta, res.Err)
	}
	// Here the ff-only merge does move the working tree, because main is checked out.
	if _, err := os.Stat(filepath.Join(clone, "two.txt")); err != nil {
		t.Errorf("ff-only merge did not update the working tree: %v", err)
	}
}

func TestUpdateAlreadyCurrent(t *testing.T) {
	_, clone := workspace(t)
	res := update(collect(clone))
	if res.Status != statusCurrent {
		t.Fatalf("Status = %q, want %q (err = %v)", res.Status, statusCurrent, res.Err)
	}
}

func TestDiscoverSkipsNonRepos(t *testing.T) {
	_, clone := workspace(t)
	root := filepath.Dir(clone)
	if err := os.Mkdir(filepath.Join(root, "not-a-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := discover(root)
	if len(got) != 1 || filepath.Base(got[0]) != "work" {
		t.Errorf("discover = %v, want just the clone", got)
	}
}

func TestRollup(t *testing.T) {
	tests := []struct {
		name   string
		checks []ghCheck
		want   string
	}{
		{"no checks", nil, ""},
		{"all green", []ghCheck{{Status: "COMPLETED", Conclusion: "SUCCESS"}}, ciPass},
		{"skipped counts as pass", []ghCheck{{Status: "COMPLETED", Conclusion: "SKIPPED"}}, ciPass},
		{"one failure wins over green", []ghCheck{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Status: "COMPLETED", Conclusion: "FAILURE"},
		}, ciFail},
		{"failure wins over pending", []ghCheck{
			{Status: "IN_PROGRESS"},
			{Status: "COMPLETED", Conclusion: "TIMED_OUT"},
		}, ciFail},
		{"in progress is pending", []ghCheck{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Status: "QUEUED"},
		}, ciPending},
		{"status context state", []ghCheck{{State: "FAILURE"}}, ciFail},
		{"status context pending", []ghCheck{{State: "PENDING"}}, ciPending},
		{"status context success", []ghCheck{{State: "SUCCESS"}}, ciPass},
	}
	for _, tc := range tests {
		if got := rollup(tc.checks); got != tc.want {
			t.Errorf("%s: rollup = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFilterRepos(t *testing.T) {
	repos := []Repo{
		{Name: "cloudscanner", Branch: "main"},
		{Name: "customer-asset-collector", Branch: "UP-5731-coverage"},
		{Name: "monolith", Branch: "main"},
	}
	tests := []struct {
		filter string
		want   int
	}{
		{"", 3},
		{"up-5731", 1}, // matches a branch, case-insensitively
		{"UP-5731", 1}, // matches a branch, as typed
		{"cloud", 1},   // matches a repo name
		{"main", 2},    // matches two branches
		{"nonsense", 0},
	}
	for _, tc := range tests {
		if got := len(filterRepos(repos, tc.filter)); got != tc.want {
			t.Errorf("filter %q = %d repos, want %d", tc.filter, got, tc.want)
		}
	}
}

// The check that catches ANSI-width bugs: turning color on must not move a
// single column. This is what text/tabwriter got wrong.
func TestRenderGridAlignmentIsColorIndependent(t *testing.T) {
	rows := [][]cell{
		{hue(dim, "REPO"), hue(dim, "BRANCH"), hue(dim, "↑↓"), hue(dim, "CI")},
		{txt("cloudscanner"), txt("main"), hue(dim, "-"), txt("")},
		{txt("customer-asset-collector"), hue(cyan, "UP-5731-coverage"), hue(yellow, "↑2↓3"), hue(green, "✓")},
		{txt("kite"), hue(yellow, "detached@39902fe"), hue(dim, "·"), hue(red, "✗")},
	}

	render := func(on bool) string {
		colorOn = on
		var b bytes.Buffer
		renderGrid(&b, rows, 2)
		return b.String()
	}
	noColor := render(false)
	colored := render(true)
	colorOn = false

	if stripped := stripANSI(colored); stripped != noColor {
		t.Errorf("color changed the layout\nplain:\n%s\ncolored, stripped:\n%s", noColor, stripped)
	}
	if !strings.Contains(colored, cyan) {
		t.Error("color was requested but no escape codes were emitted")
	}
	for _, line := range strings.Split(strings.TrimRight(noColor, "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("row carries trailing whitespace: %q", line)
		}
	}
}

func TestRenderGridRaggedRows(t *testing.T) {
	colorOn = false
	var b bytes.Buffer
	renderGrid(&b, [][]cell{
		{txt("a"), txt("bb"), txt("ccc")},
		{txt("dddd")}, // shorter row must not panic or pad past its own end
	}, 1)
	want := "a    bb ccc\ndddd\n"
	if got := b.String(); got != want {
		t.Errorf("renderGrid = %q, want %q", got, want)
	}
}

func TestPlural(t *testing.T) {
	tests := []struct {
		n          int
		word, want string
	}{
		{1, "repo", "1 repo"},
		{2, "repo", "2 repos"},
		{1, "stash", "1 stash"},
		{3, "stash", "3 stashes"},
	}
	for _, tc := range tests {
		if got := plural(tc.n, tc.word); got != tc.want {
			t.Errorf("plural(%d, %q) = %q, want %q", tc.n, tc.word, got, tc.want)
		}
	}
}
