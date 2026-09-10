package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
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
// The commit lands on origin before the real clone, exactly like a normal
// clone of a populated remote, so the clone gets a real refs/remotes/
// origin/HEAD. TestDefaultBranchFallsBackWithoutOriginHEAD covers the case
// where that ref is absent.
func workspace(t *testing.T) (origin, clone string) {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()

	origin = filepath.Join(root, "origin.git")
	mustGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	addRemoteCommit(t, origin, "one.txt")

	clone = filepath.Join(root, "work")
	mustGit(t, root, "clone", "-q", origin, clone)
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

	r := collect(clone, true)
	if r.Err != nil {
		t.Fatalf("collect: %v", r.Err)
	}
	if r.Branch != "main" {
		t.Errorf("Branch = %q, want main", r.Branch)
	}
	if r.Default != "main" {
		t.Errorf("Default = %q, want main", r.Default)
	}
	if r.Detached {
		t.Error("Detached = true on a named branch")
	}
	if r.Dirty != 2 {
		t.Errorf("Dirty = %d, want 2", r.Dirty)
	}
	if r.NoUpstream {
		t.Error("NoUpstream = true, want false: a fresh clone's branch already tracks origin/main")
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

	r := collect(clone, true)
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

	res := update(collect(clone, true))
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

	res := update(collect(clone, true))
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

	res := update(collect(clone, true))
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
	res := update(collect(clone, true))
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
		{Name: "prometheus", Branch: "main"},
		{Name: "grafana", Branch: "PROJ-118-retry-backoff"},
		{Name: "terraform", Branch: "main"},
	}
	tests := []struct {
		filter string
		want   int
	}{
		{"", 3},
		{"proj-118", 1}, // matches a branch, case-insensitively
		{"PROJ-118", 1}, // matches a branch, as typed
		{"graf", 1},     // matches a repo name
		{"main", 2},     // matches two branches
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
		{txt("prometheus"), txt("main"), hue(dim, "-"), txt("")},
		{txt("grafana"), hue(cyan, "PROJ-118-retry-backoff"), hue(yellow, "↑2↓3"), hue(green, "✓")},
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

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args    []string
		want    opts
		wantErr bool
	}{
		{args: nil, want: opts{cmd: "status"}},
		{args: []string{"PROJ-118"}, want: opts{cmd: "status", filter: "PROJ-118"}},
		{args: []string{"update"}, want: opts{cmd: "update"}},
		{args: []string{"update", "kite"}, want: opts{cmd: "update", filter: "kite"}},
		{args: []string{"--root", "/tmp/x"}, want: opts{cmd: "status", root: "/tmp/x"}},
		{args: []string{"--root=/tmp/x"}, want: opts{cmd: "status", root: "/tmp/x"}},
		{args: []string{"-root=/tmp/x"}, want: opts{cmd: "status", root: "/tmp/x"}},
		// A filter after the flag's value must not be swallowed by it.
		{args: []string{"--root", "/tmp/x", "update", "api"}, want: opts{cmd: "update", filter: "api", root: "/tmp/x"}},
		{args: []string{"--no-pr", "--root", "/tmp/x"}, want: opts{cmd: "status", root: "/tmp/x", noPR: true}},
		{args: []string{"status", "--no-pr"}, want: opts{cmd: "status", noPR: true}},
		{args: []string{"--help"}, want: opts{cmd: "help"}},
		{args: []string{"--version"}, want: opts{cmd: "version"}},
		// A bare command wins over being read as a filter.
		{args: []string{"version"}, want: opts{cmd: "version"}},
		{args: []string{"--root"}, wantErr: true},
		{args: []string{"--root="}, wantErr: true},
		{args: []string{"--bogus"}, wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseArgs(%q) = %+v, want error", tc.args, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseArgs(%q): %v", tc.args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestRootDirDefaultsToWorkingDirectory(t *testing.T) {
	if got := rootDir("/tmp/somewhere"); got != "/tmp/somewhere" {
		t.Errorf("rootDir(flag) = %q, want the flag value", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := rootDir(""); got != wd {
		t.Errorf("rootDir(\"\") = %q, want the working directory %q", got, wd)
	}
}

// Someone without gh installed or authenticated gets no PR data, so they must
// not see two dead columns.
func TestPrintTableHidesPRColumnsWithNoData(t *testing.T) {
	colorOn = false
	repos := []Repo{{Name: "prometheus", Branch: "main", Default: "main"}}

	var b bytes.Buffer
	printTable(&b, repos, false, "")
	out := b.String()

	for _, col := range []string{"PR", "CI"} {
		if strings.Contains(out, col) {
			t.Errorf("header still shows %s with no PR data:\n%s", col, out)
		}
	}
}

func TestPrintTableShowsPRColumnsWhenPopulated(t *testing.T) {
	colorOn = false
	repos := []Repo{
		{Name: "prometheus", Branch: "main", Default: "main"},
		{Name: "grafana", Branch: "feat/retry-backoff", Default: "main", PR: &PR{Number: 4211, CI: ciFail}},
	}

	var b bytes.Buffer
	printTable(&b, repos, false, "")
	out := b.String()

	for _, want := range []string{"PR", "CI", "#4211", "✗"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A PR with no checks at all should show the PR column but not a dead CI column.
func TestPrintTableHidesCIWhenNoChecks(t *testing.T) {
	colorOn = false
	repos := []Repo{{Name: "vault", Branch: "fix/1234-nil-deref", Default: "main", PR: &PR{Number: 77}}}

	var b bytes.Buffer
	printTable(&b, repos, false, "")
	out := b.String()

	if !strings.Contains(out, "#77") {
		t.Errorf("PR column missing:\n%s", out)
	}
	if strings.Contains(out, "CI") {
		t.Errorf("CI column shown with no check data:\n%s", out)
	}
}

func TestPrintTableFooterNote(t *testing.T) {
	colorOn = false
	var b bytes.Buffer
	printTable(&b, []Repo{{Name: "etcd", Branch: "main", Default: "main"}}, false, "gh not installed, PR columns hidden")
	if !strings.Contains(b.String(), "gh not installed") {
		t.Errorf("footer note missing:\n%s", b.String())
	}
}

// TestRenderGridAlignmentWithSubRows exercises renderGrid directly and never
// reaches printTable, so it cannot catch a broken sub-row loop. This does.
// colGaps splits a rendered table line into its columns. Columns are
// separated by two or more spaces (the grid's gap, plus any padding to the
// widest cell in that column), so a cell that renders entirely blank simply
// contributes no field at all rather than an empty string in its place.
// That is the signal TestPrintTableRendersSubRowsForOtherBranchPRs uses to
// tell "vs MAIN rendered blank" apart from "vs MAIN rendered -".
func colGaps(line string) []string {
	return regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1)
}

func TestPrintTableRendersSubRowsForOtherBranchPRs(t *testing.T) {
	colorOn = false
	newer := time.Now()
	older := newer.Add(-48 * time.Hour)

	repos := []Repo{{
		Name: "caddy", Branch: "main", Default: "main",
		PR: &PR{Number: 100, Review: revApproved, CI: ciPass},
		Others: []PRRow{
			// Already newest-first: this is the order fillRows hands to
			// printTable upstream. printTable must preserve it, not reorder,
			// and using two entries (not one) catches a pointer-aliasing bug
			// a single entry would hide. Resolved: true is the reachable
			// state for an ordinary tracked branch level with its upstream.
			{Branch: "feat/newer", LastCommit: newer, Resolved: true, PR: PR{Number: 501, Review: revChanges, CI: ciFail}},
			{Branch: "feat/older", LastCommit: older, Resolved: true, PR: PR{Number: 502, Review: revRequired, CI: ciPass}},
			// Resolved, but pushed without -u: no @{upstream} to compare
			// against, though vs MAIN is still known from the resolved ref.
			{Branch: "feat/no-upstream", LastCommit: newer, NoUpstream: true, Resolved: true, BehindMain: 3, PR: PR{Number: 503}},
			// Never resolved to any ref at all. fillRows always pairs an
			// unresolved branch with NoUpstream: true, so Resolved: false,
			// NoUpstream: false (the old fixture's combination) is not a
			// state fillRows can actually produce.
			{Branch: "feat/unresolved", LastCommit: newer, NoUpstream: true, PR: PR{Number: 504}},
		},
	}}

	var b bytes.Buffer
	printTable(&b, repos, false, "")
	out := b.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if !strings.Contains(lines[0], "RV") {
		t.Errorf("RV header missing, a sub-row's review state should switch showRV on:\n%s", out)
	}
	// CI must stay the last column: its raw cell is only safe unpadded as a
	// row's final cell, so a column added after it would silently misalign
	// every row with a failing check.
	if !strings.HasSuffix(lines[0], "CI") {
		t.Errorf("CI is not the last header column: %q", lines[0])
	}

	findLine := func(needle string) (int, string) {
		for i, l := range lines {
			if strings.Contains(l, needle) {
				return i, l
			}
		}
		t.Fatalf("no sub-row rendered for %q:\n%s", needle, out)
		return -1, ""
	}

	newerLine, newerText := findLine("└ feat/newer")
	olderLine, olderText := findLine("└ feat/older")
	if newerLine >= olderLine {
		t.Errorf("sub-rows out of order: want feat/newer (newest commit) before feat/older, got newer at line %d, older at line %d", newerLine, olderLine)
	}
	if !strings.Contains(newerText, "#501") {
		t.Errorf("feat/newer sub-row missing #501: %q", newerText)
	}
	if !strings.Contains(olderText, "#502") {
		t.Errorf("feat/older sub-row missing #502: %q", olderText)
	}
	if strings.Contains(newerText, "#502") || strings.Contains(olderText, "#501") {
		t.Errorf("PR numbers bled across sub-rows:\n%q\n%q", newerText, olderText)
	}

	// The wiring finding 4 fixed: o.NoUpstream flows into the ↑↓ cell, and
	// o.Resolved gates whether vs MAIN shows a real value or renders blank
	// (via subRowMainCell, main.go:501-502). Columns: [branch, DIRTY, ↑↓,
	// vs MAIN, STASH, LAST, PR, ...].
	_, noUpstreamText := findLine("└ feat/no-upstream")
	noUpstreamCols := colGaps(noUpstreamText)
	if len(noUpstreamCols) < 4 {
		t.Fatalf("feat/no-upstream sub-row has too few columns: %v", noUpstreamCols)
	}
	if noUpstreamCols[2] != "·" {
		t.Errorf("feat/no-upstream ↑↓ = %q, want \"·\" (o.NoUpstream not wired into upstreamCell)", noUpstreamCols[2])
	}
	if noUpstreamCols[3] != "-3" {
		t.Errorf("feat/no-upstream vs MAIN = %q, want \"-3\" (a resolved branch must still report BehindMain)", noUpstreamCols[3])
	}

	_, unresolvedText := findLine("└ feat/unresolved")
	unresolvedCols := colGaps(unresolvedText)
	if len(unresolvedCols) < 3 {
		t.Fatalf("feat/unresolved sub-row has too few columns: %v", unresolvedCols)
	}
	if unresolvedCols[2] != "·" {
		t.Errorf("feat/unresolved ↑↓ = %q, want \"·\"", unresolvedCols[2])
	}
	// vs MAIN blank means it contributes no column at all: branch, DIRTY,
	// ↑↓, STASH, LAST, PR is 6 fields. If subRowMainCell's blank-on-
	// unresolved result stopped reaching printTable, vs MAIN would render
	// "-" as its own column instead, making 7.
	if len(unresolvedCols) != 6 {
		t.Errorf("feat/unresolved sub-row has %d columns %v, want 6 (vs MAIN must render blank, not \"-\")", len(unresolvedCols), unresolvedCols)
	}

	if !strings.Contains(out, "5 open PRs") {
		t.Errorf("footer should count the repo's own PR plus all four sub-rows (5 open PRs):\n%s", out)
	}
}

func TestRenderGridRawTrailingCell(t *testing.T) {
	colorOn = false
	var b bytes.Buffer
	renderGrid(&b, [][]cell{
		{txt("a"), txt("short"), rawCell("free text here")},
		{txt("bb"), txt("much longer cell"), rawCell("x")},
	}, 2)
	// The raw cell must not be padded out to match the other row's raw cell.
	want := "a   short             free text here\nbb  much longer cell  x\n"
	if got := b.String(); got != want {
		t.Errorf("renderGrid with raw cells =\n%q\nwant\n%q", got, want)
	}
}

func TestVersion(t *testing.T) {
	got := version()
	if got == "" {
		t.Fatal("version() returned empty string")
	}
	if strings.ContainsAny(got, " \t\n") {
		t.Errorf("version() = %q, want a single token", got)
	}
}

func TestVersionPrefersBuildStamp(t *testing.T) {
	old := buildVersion
	t.Cleanup(func() { buildVersion = old })

	buildVersion = "v9.9.9"
	if got := version(); got != "v9.9.9" {
		t.Errorf("version() = %q, want the ldflags stamp v9.9.9", got)
	}
}

// stalefixture builds a clone holding one branch of each interesting shape and
// returns them keyed by name. The gone-branch reproduces what a squash merge
// leaves behind: a pushed branch whose remote counterpart was then deleted,
// with no ancestry linking it to main.
func staleFixture(t *testing.T) (clone string, got map[string]Branch) {
	t.Helper()
	_, clone = workspace(t)

	// Merged by ancestry: the branch's commit lands on main, main is pushed.
	mustGit(t, clone, "switch", "-qc", "merged-branch")
	writeFile(t, clone, "merged.txt", "merged")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "merged work")
	mustGit(t, clone, "switch", "-q", "main")
	mustGit(t, clone, "merge", "-q", "--ff-only", "merged-branch")
	mustGit(t, clone, "push", "-q", "origin", "main")

	// Upstream gone, no ancestry: the squash-merge shape.
	mustGit(t, clone, "switch", "-qc", "gone-branch")
	writeFile(t, clone, "gone.txt", "gone")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "gone work")
	mustGit(t, clone, "push", "-q", "-u", "origin", "gone-branch")
	mustGit(t, clone, "push", "-q", "origin", "--delete", "gone-branch")

	// Still live: pushed and the remote branch is still there.
	mustGit(t, clone, "switch", "-q", "main")
	mustGit(t, clone, "switch", "-qc", "active-branch")
	writeFile(t, clone, "active.txt", "active")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "active work")
	mustGit(t, clone, "push", "-q", "-u", "origin", "active-branch")

	mustGit(t, clone, "switch", "-q", "main")

	got = map[string]Branch{}
	for _, b := range staleBranches(collect(clone, true)) {
		if b.Err != nil {
			t.Fatalf("staleBranches: %v", b.Err)
		}
		got[b.Name] = b
	}
	return clone, got
}

func TestStaleBranchesClassification(t *testing.T) {
	_, got := staleFixture(t)

	if len(got) != 2 {
		t.Fatalf("found %d stale branches %v, want merged-branch and gone-branch only", len(got), keysOf(got))
	}
	if _, ok := got["active-branch"]; ok {
		t.Error("a branch with a live upstream and no ancestry must not be listed")
	}
	if _, ok := got["main"]; ok {
		t.Error("the default branch must never be listed")
	}

	m := got["merged-branch"]
	if !m.Merged || m.Gone || m.Current {
		t.Errorf("merged-branch: Merged=%v Gone=%v Current=%v, want true/false/false", m.Merged, m.Gone, m.Current)
	}
	if m.verdict() != pruneMerged || !m.deletable(false) {
		t.Errorf("merged-branch: verdict=%q deletable=%v, want %q/true", m.verdict(), m.deletable(false), pruneMerged)
	}

	// This is the case a plain `git branch --merged` cannot see at all.
	g := got["gone-branch"]
	if g.Merged {
		t.Error("gone-branch: a squash-merged branch has no ancestry, Merged must be false")
	}
	if !g.Gone {
		t.Error("gone-branch: Gone must be true once the deleted upstream is pruned")
	}
	if g.verdict() != pruneUnsure {
		t.Errorf("gone-branch: verdict=%q, want %q with no merged PR to confirm it", g.verdict(), pruneUnsure)
	}
	if g.deletable(false) {
		t.Error("gone-branch: an unconfirmed merge must not be deletable without force")
	}
	if !g.deletable(true) {
		t.Error("gone-branch: force must allow it")
	}
}

func TestDeleteBranch(t *testing.T) {
	clone, got := staleFixture(t)

	if err := deleteBranch(got["merged-branch"]); err != nil {
		t.Fatalf("deleteBranch(merged-branch): %v", err)
	}
	if out := mustGit(t, clone, "branch", "--list", "merged-branch"); out != "" {
		t.Errorf("merged-branch survived deletion: %q", out)
	}
	// Everything else must be untouched.
	for _, b := range []string{"gone-branch", "active-branch", "main"} {
		if out := mustGit(t, clone, "branch", "--list", b); out == "" {
			t.Errorf("%s was deleted but should not have been", b)
		}
	}

	// A squash-merged branch has no ancestry, so -d would refuse it and
	// deleteBranch must reach for -D.
	if err := deleteBranch(got["gone-branch"]); err != nil {
		t.Fatalf("deleteBranch(gone-branch): %v", err)
	}
	if out := mustGit(t, clone, "branch", "--list", "gone-branch"); out != "" {
		t.Errorf("gone-branch survived deletion: %q", out)
	}
}

func TestCheckedOutBranchIsNeverDeletable(t *testing.T) {
	_, clone := workspace(t)
	mustGit(t, clone, "switch", "-qc", "gone-branch")
	writeFile(t, clone, "gone.txt", "gone")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "gone work")
	mustGit(t, clone, "push", "-q", "-u", "origin", "gone-branch")
	mustGit(t, clone, "push", "-q", "origin", "--delete", "gone-branch")
	// Deliberately stay on gone-branch.

	var b Branch
	for _, got := range staleBranches(collect(clone, true)) {
		if got.Name == "gone-branch" {
			b = got
		}
	}
	if !b.Current {
		t.Fatalf("Current=false for the checked-out branch (Gone=%v)", b.Gone)
	}
	if b.verdict() != pruneCurrent {
		t.Errorf("verdict=%q, want %q", b.verdict(), pruneCurrent)
	}
	if b.deletable(true) {
		t.Error("even --force must not delete the checked-out branch, kite never switches branches")
	}
}

func TestStashList(t *testing.T) {
	_, clone := workspace(t)
	if got := stashList(collect(clone, true)); len(got) != 0 {
		t.Fatalf("a clean repo reported %d stashes", len(got))
	}

	writeFile(t, clone, "one.txt", "modified")
	mustGit(t, clone, "stash", "push", "-m", "keep this for later")

	got := stashList(collect(clone, true))
	if len(got) != 1 {
		t.Fatalf("got %d stashes, want 1", len(got))
	}
	if got[0].Ref != "stash@{0}" {
		t.Errorf("Ref = %q, want stash@{0}", got[0].Ref)
	}
	if !strings.Contains(got[0].Subject, "keep this for later") {
		t.Errorf("Subject = %q, want it to carry the message", got[0].Subject)
	}
	if got[0].Age == "" {
		t.Error("Age is empty")
	}
	if got[0].Repo != "work" {
		t.Errorf("Repo = %q, want work", got[0].Repo)
	}
}

func TestRepoPaths(t *testing.T) {
	paths := []string{"/w/api-gateway", "/w/Billing", "/w/web"}
	tests := []struct {
		filter string
		want   int
	}{
		{"", 3},
		{"api", 1},
		{"bill", 1}, // case-insensitive against the directory name
		{"w", 2},    // api-gateway and web, but NOT the /w/ parent directory
		{"nope", 0},
	}
	for _, tc := range tests {
		if got := len(repoPaths(paths, tc.filter)); got != tc.want {
			t.Errorf("repoPaths(%q) = %d, want %d", tc.filter, got, tc.want)
		}
	}
}

func keysOf(m map[string]Branch) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestReviewState(t *testing.T) {
	cases := []struct {
		decision string
		draft    bool
		want     string
	}{
		{"APPROVED", false, revApproved},
		{"CHANGES_REQUESTED", false, revChanges},
		{"REVIEW_REQUIRED", false, revRequired},
		{"", false, ""},
		// Draft wins over any decision: a draft is not asking for review.
		{"APPROVED", true, revDraft},
		{"", true, revDraft},
		{"SOMETHING_NEW", false, ""},
	}
	for _, c := range cases {
		if got := reviewState(c.decision, c.draft); got != c.want {
			t.Errorf("reviewState(%q, %v) = %q, want %q", c.decision, c.draft, got, c.want)
		}
	}
}

func TestFirstFailing(t *testing.T) {
	cases := []struct {
		name      string
		checks    []ghCheck
		wantName  string
		wantExtra int
	}{
		{"none", []ghCheck{{Conclusion: "SUCCESS", Name: "build"}}, "", 0},
		{
			"one CheckRun",
			[]ghCheck{{Conclusion: "FAILURE", Name: "lint"}},
			"lint", 0,
		},
		{
			// Sorted, so the reported name cannot change between runs on
			// whatever order the API happened to return.
			"sorted across shapes",
			[]ghCheck{
				{Conclusion: "FAILURE", Name: "vet"},
				{State: "FAILURE", Context: "lint"},
				{Conclusion: "SUCCESS", Name: "build"},
				{Conclusion: "TIMED_OUT", Name: "e2e"},
			},
			"e2e", 2,
		},
		{
			"unnamed check still reports",
			[]ghCheck{{Conclusion: "FAILURE"}},
			"check", 0,
		},
	}
	for _, c := range cases {
		gotName, gotExtra := firstFailing(c.checks)
		if gotName != c.wantName || gotExtra != c.wantExtra {
			t.Errorf("%s: firstFailing = (%q, %d), want (%q, %d)",
				c.name, gotName, gotExtra, c.wantName, c.wantExtra)
		}
	}
}

func TestSplitPRs(t *testing.T) {
	list := []ghPR{
		{Number: 10, HeadRefName: "feature", ReviewDecision: "APPROVED",
			StatusCheckRollup: []ghCheck{{Conclusion: "SUCCESS", Name: "build"}}},
		{Number: 11, HeadRefName: "other", IsDraft: true},
		{Number: 12, HeadRefName: "third", ReviewDecision: "REVIEW_REQUIRED",
			StatusCheckRollup: []ghCheck{
				{Conclusion: "FAILURE", Name: "lint"},
				{Conclusion: "FAILURE", Name: "vet"},
			}},
	}

	own, others := splitPRs("feature", list)
	if own == nil {
		t.Fatal("checked-out branch got no PR")
	}
	if own.Number != 10 || own.Review != revApproved || own.CI != ciPass {
		t.Errorf("own = %+v, want #10 approved passing", *own)
	}
	if len(others) != 2 {
		t.Fatalf("got %d sub-rows, want 2", len(others))
	}

	byBranch := map[string]PRRow{}
	for _, o := range others {
		byBranch[o.Branch] = o
	}
	if got := byBranch["other"].PR.Review; got != revDraft {
		t.Errorf("draft PR review = %q, want %q", got, revDraft)
	}
	third := byBranch["third"].PR
	if third.CI != ciFail || third.Failing != "lint" || third.Extra != 1 {
		t.Errorf("third = %+v, want failing lint +1", third)
	}
}

func TestSplitPRsNoneOnCurrentBranch(t *testing.T) {
	// A repo sitting on main with an open PR elsewhere: the whole reason
	// sub-rows exist. The main row must stay PR-less, not borrow one.
	own, others := splitPRs("main", []ghPR{{Number: 7, HeadRefName: "feature"}})
	if own != nil {
		t.Errorf("own = %+v, want nil", *own)
	}
	if len(others) != 1 || others[0].Branch != "feature" {
		t.Errorf("others = %+v, want one row for feature", others)
	}
}

func TestRefsReadsBranchesNotCheckedOut(t *testing.T) {
	origin, clone := workspace(t)
	_ = origin

	// A second local branch, one commit ahead of main, left un-checked-out.
	mustGit(t, clone, "checkout", "-q", "-b", "feature")
	writeFile(t, clone, "two.txt", "two")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "two")
	mustGit(t, clone, "push", "-q", "-u", "origin", "feature")
	mustGit(t, clone, "checkout", "-q", "main")

	m := refs(clone)
	feat, ok := m["feature"]
	if !ok {
		t.Fatalf("feature missing from refs: %v", m)
	}
	if feat.Upstream != "origin/feature" {
		t.Errorf("upstream = %q, want origin/feature", feat.Upstream)
	}
	if feat.LastCommit.IsZero() {
		t.Error("feature has no commit date")
	}
	if _, ok := m["origin/feature"]; !ok {
		t.Errorf("origin/feature missing from refs: %v", m)
	}
}

func TestRefForPrefersLocalThenOrigin(t *testing.T) {
	m := map[string]refMeta{
		"local-only":         {},
		"origin/both":        {},
		"both":               {},
		"origin/remote-only": {},
	}
	cases := map[string]string{
		"local-only":  "local-only",
		"both":        "both",
		"remote-only": "origin/remote-only",
		"nowhere":     "",
	}
	for in, want := range cases {
		if got := refFor(in, m); got != want {
			t.Errorf("refFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectRecordsLocalNonDefaultBranches(t *testing.T) {
	_, clone := workspace(t)
	mustGit(t, clone, "branch", "feature")
	mustGit(t, clone, "branch", "spike")

	r := collect(clone, true)
	got := strings.Join(r.LocalBranches, ",")
	if got != "feature,spike" {
		t.Errorf("LocalBranches = %q, want \"feature,spike\" (main excluded)", got)
	}
}

// TestDefaultBranchFallsBackWithoutOriginHEAD covers the case workspace no
// longer exercises by default now that its origin is populated before the
// clone: a real clone that, for whatever reason, never got origin/HEAD set.
func TestDefaultBranchFallsBackWithoutOriginHEAD(t *testing.T) {
	_, clone := workspace(t)
	mustGit(t, clone, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")

	if got := defaultBranch(clone); got != "main" {
		t.Errorf("defaultBranch = %q, want main via the probe fallback", got)
	}
}

// TestCollectOnRealCloneDoesNotMatchBareOrigin is the regression case for the
// filter bug: a normal clone's for-each-ref renders refs/remotes/origin/HEAD
// as the bare "origin" (no slash), which used to slip past the origin/ prefix
// check and land in LocalBranches, making a filter like "ori" or "in" match
// every repo in the workspace.
func TestCollectOnRealCloneDoesNotMatchBareOrigin(t *testing.T) {
	_, clone := workspace(t)

	r := collect(clone, true)
	if _, ok := r.Refs["origin"]; !ok {
		t.Fatal("test invalid: this clone never got a bare \"origin\" ref from origin/HEAD")
	}
	for _, b := range r.LocalBranches {
		if b == "origin" {
			t.Fatalf("LocalBranches = %v, contains the bare origin/HEAD ref", r.LocalBranches)
		}
	}
	// "main" legitimately contains "in", so it is excluded here: these three
	// substrings of "origin" are exactly what the bug made match every repo.
	for _, f := range []string{"ori", "gin", "rig"} {
		if matchesFilter(r, f) {
			t.Errorf("matchesFilter(%q) = true on a repo with no branch or name containing %q", f, f)
		}
	}
}

// TestRefForResolvesDeletedLocalBranchViaOrigin is the load-bearing path: a
// PR's head branch that was never cloned here, or has since been deleted, so
// it resolves only through origin/<branch>.
func TestRefForResolvesDeletedLocalBranchViaOrigin(t *testing.T) {
	origin, clone := workspace(t)

	mustGit(t, clone, "checkout", "-q", "-b", "gone-local")
	writeFile(t, clone, "gone.txt", "gone")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "gone-local work")
	mustGit(t, clone, "push", "-q", "-u", "origin", "gone-local")
	mustGit(t, clone, "checkout", "-q", "main")
	mustGit(t, clone, "branch", "-D", "gone-local")

	// Advance origin/main so BehindMain has something real to compute.
	addRemoteCommit(t, origin, "after-branch.txt")
	mustGit(t, clone, "fetch", "-q", "origin")

	m := refs(clone)
	if ref := refFor("gone-local", m); ref != "origin/gone-local" {
		t.Fatalf("refFor(%q) = %q, want origin/gone-local", "gone-local", ref)
	}

	rows := []PRRow{{Branch: "gone-local"}}
	fillRows(clone, "main", rows, m)
	if rows[0].LastCommit.IsZero() {
		t.Error("LastCommit not filled from the origin-only ref")
	}
	if rows[0].BehindMain != 1 {
		t.Errorf("BehindMain = %d, want 1", rows[0].BehindMain)
	}
}

// TestFillRowsNoUpstreamWhenPushedWithoutDashU is the first case from the
// sub-row rendering bug: a local branch pushed without -u has no @{upstream},
// so parseTrack reads 0/0 and used to render as "level with upstream" rather
// than "unknown".
func TestFillRowsNoUpstreamWhenPushedWithoutDashU(t *testing.T) {
	_, clone := workspace(t)

	mustGit(t, clone, "checkout", "-q", "-b", "no-track")
	writeFile(t, clone, "no-track.txt", "no-track")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "no-track work")
	mustGit(t, clone, "push", "-q", "origin", "no-track") // no -u: no upstream configured
	mustGit(t, clone, "checkout", "-q", "main")

	m := refs(clone)
	rows := []PRRow{{Branch: "no-track"}}
	fillRows(clone, "main", rows, m)

	if !rows[0].Resolved {
		t.Fatal("Resolved = false, want true: the local branch exists")
	}
	if !rows[0].NoUpstream {
		t.Error("NoUpstream = false, want true: pushed without -u leaves no @{upstream}")
	}
}

// TestFillRowsUnresolvedBranchStaysUnresolved is the second case: a branch
// refFor cannot find anywhere used to leave BehindMain at its zero value,
// which renders as "level with main" instead of "unknown".
func TestFillRowsUnresolvedBranchStaysUnresolved(t *testing.T) {
	_, clone := workspace(t)
	m := refs(clone)

	rows := []PRRow{{Branch: "never-existed"}}
	fillRows(clone, "main", rows, m)

	if rows[0].Resolved {
		t.Error("Resolved = true for a branch with neither a local nor origin ref")
	}
	if !rows[0].NoUpstream {
		t.Error("NoUpstream = false, want true when nothing at all is known about the branch")
	}
}

func TestConflictsDetectsAndLeavesTreeAlone(t *testing.T) {
	origin, clone := workspace(t)

	// Advance origin/main first with an unrelated file; the real conflicting
	// edit to one.txt comes later, from the second clone below.
	addRemoteCommit(t, origin, "remote")
	mustGit(t, clone, "checkout", "-q", "-b", "feature")
	writeFile(t, clone, "one.txt", "feature version")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "feature edits one.txt")
	mustGit(t, clone, "fetch", "-q", "origin")

	// Make origin/main touch the same file so the merge genuinely conflicts.
	tmp := t.TempDir()
	other := filepath.Join(tmp, "other")
	mustGit(t, tmp, "clone", "-q", origin, other)
	writeFile(t, other, "one.txt", "origin version")
	mustGit(t, other, "add", ".")
	mustGit(t, other, "commit", "-qm", "origin edits one.txt")
	mustGit(t, other, "push", "-q", "origin", "main")
	mustGit(t, clone, "fetch", "-q", "origin")

	writeFile(t, clone, "dirty.txt", "PRECIOUS")

	if !conflicts(clone, "feature", "main") {
		t.Error("conflicts = false, want true for two edits to the same file")
	}

	// The whole feature rests on this: predicting a conflict must not create
	// one, move the branch, or disturb uncommitted work.
	if got := mustGit(t, clone, "branch", "--show-current"); got != "feature" {
		t.Errorf("branch moved to %q", got)
	}
	body, err := os.ReadFile(filepath.Join(clone, "dirty.txt"))
	if err != nil || strings.TrimSpace(string(body)) != "PRECIOUS" {
		t.Errorf("uncommitted file damaged: %q, %v", body, err)
	}
	if st := mustGit(t, clone, "status", "--porcelain"); !strings.Contains(st, "dirty.txt") {
		t.Errorf("status lost the untracked file: %q", st)
	}
	// The file actually in conflict must be untouched: a real merge or rebase
	// would rewrite it with conflict markers.
	one, err := os.ReadFile(filepath.Join(clone, "one.txt"))
	if err != nil || strings.TrimSpace(string(one)) != "feature version" {
		t.Errorf("one.txt changed: %q, %v", one, err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Errorf("MERGE_HEAD present, a real merge was left in progress: %v", err)
	}
}

func TestConflictsFalseWhenMergeIsClean(t *testing.T) {
	origin, clone := workspace(t)

	// origin/main adds a different file, so nothing overlaps.
	addRemoteCommit(t, origin, "remote")
	mustGit(t, clone, "checkout", "-q", "-b", "feature")
	writeFile(t, clone, "feature-only.txt", "mine")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "feature adds its own file")
	mustGit(t, clone, "fetch", "-q", "origin")

	if conflicts(clone, "feature", "main") {
		t.Error("conflicts = true, want false for non-overlapping changes")
	}
}

func TestConflictsFalseForUnknownRef(t *testing.T) {
	_, clone := workspace(t)
	if conflicts(clone, "", "main") {
		t.Error("an unresolvable ref must report no conflict, not a conflict")
	}
	if conflicts(clone, "feature", "") {
		t.Error("an unresolvable default must report no conflict, not a conflict")
	}
}

func TestParseTrack(t *testing.T) {
	cases := []struct {
		in     string
		ahead  int
		behind int
		gone   bool
	}{
		{"", 0, 0, false},
		{"[ahead 2]", 2, 0, false},
		{"[behind 3]", 0, 3, false},
		{"[ahead 1, behind 4]", 1, 4, false},
		{"[gone]", 0, 0, true},
	}
	for _, c := range cases {
		a, b, gone := parseTrack(c.in)
		if a != c.ahead || b != c.behind || gone != c.gone {
			t.Errorf("parseTrack(%q) = (%d, %d, %v), want (%d, %d, %v)",
				c.in, a, b, gone, c.ahead, c.behind, c.gone)
		}
	}
}

func TestFilterMatchesLocalNonDefaultBranch(t *testing.T) {
	repos := []Repo{
		{Name: "caddy", Branch: "main", Default: "main", LocalBranches: []string{"UP-6248-armid"}},
		{Name: "grafana", Branch: "main", Default: "main"},
	}
	got := filterRepos(repos, "up-6248")
	if len(got) != 1 || got[0].Name != "caddy" {
		t.Errorf("filter did not find the un-checked-out branch: %+v", got)
	}
}

func TestFilterMainDoesNotMatchEveryRepo(t *testing.T) {
	// The reason remote refs are excluded from LocalBranches: every repo has
	// an origin/main, so matching remotes would make this filter useless.
	repos := []Repo{
		{Name: "caddy", Branch: "feature", Default: "main", LocalBranches: []string{"feature"}},
		{Name: "grafana", Branch: "main", Default: "main"},
	}
	got := filterRepos(repos, "main")
	if len(got) != 1 || got[0].Name != "grafana" {
		t.Errorf("filter \"main\" = %+v, want only the repo actually on main", got)
	}
}

func TestReviewCellGlyphs(t *testing.T) {
	colorOn = false
	cases := []struct {
		pr   *PR
		want string
	}{
		{nil, ""},
		{&PR{Review: revApproved}, "✓"},
		{&PR{Review: revChanges}, "✗"},
		{&PR{Review: revRequired}, "·"},
		{&PR{Review: revDraft}, "✎"},
		{&PR{}, ""},
	}
	for _, c := range cases {
		if got := reviewCell(c.pr).String(); got != c.want {
			t.Errorf("reviewCell(%+v) = %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestCICellNamesTheFailingCheck(t *testing.T) {
	colorOn = false
	cases := []struct {
		pr   *PR
		want string
	}{
		{&PR{CI: ciPass}, "✓"},
		{&PR{CI: ciPending}, "○"},
		{&PR{CI: ciFail}, "✗"},
		{&PR{CI: ciFail, Failing: "lint"}, "✗ lint"},
		{&PR{CI: ciFail, Failing: "lint", Extra: 2}, "✗ lint +2"},
		// Longer than truncate's 20-rune cap, so this also pins where "+N"
		// lands relative to the ellipsis.
		{&PR{CI: ciFail, Failing: "build (ubuntu-latest, go 1.26)", Extra: 3}, "✗ build (ubuntu-lates… +3"},
	}
	for _, c := range cases {
		if got := ciCell(c.pr).String(); got != c.want {
			t.Errorf("ciCell(%+v) = %q, want %q", c.pr, got, c.want)
		}
	}

	// A raw cell must measure zero, or it pads the column it sits in.
	if w := ciCell(&PR{CI: ciFail, Failing: "lint"}).width(); w != 0 {
		t.Errorf("raw CI cell width = %d, want 0", w)
	}
}

func TestBehindMainCellConflictMarker(t *testing.T) {
	colorOn = false
	if got := behindMainCell(0, false).String(); got != "-" {
		t.Errorf("behindMainCell(0) = %q, want \"-\"", got)
	}
	if got := behindMainCell(14, false).String(); got != "-14" {
		t.Errorf("behindMainCell(14, false) = %q, want \"-14\"", got)
	}
	got := behindMainCell(14, true).String()
	if got != "-14!" {
		t.Errorf("behindMainCell(14, true) = %q, want \"-14!\"", got)
	}
	// "!" is ASCII on purpose. A double-width glyph would shift every column
	// to its right, because width() counts runes rather than display cells.
	if len([]rune(got)) != len(got) {
		t.Errorf("conflict marker is not ASCII: %q", got)
	}
}

func TestSubRowMainCellBlankWhenUnresolved(t *testing.T) {
	colorOn = false
	if got := subRowMainCell(&PRRow{Resolved: false, BehindMain: 4}).String(); got != "" {
		t.Errorf("subRowMainCell(unresolved) = %q, want blank", got)
	}
	if got := subRowMainCell(&PRRow{Resolved: true, BehindMain: 4}).String(); got != "-4" {
		t.Errorf("subRowMainCell(resolved) = %q, want \"-4\"", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("lint", 10); got != "lint" {
		t.Errorf("truncate short = %q", got)
	}
	got := truncate("build (ubuntu-latest, go 1.26)", 10)
	if want := "build (ub…"; got != want {
		t.Errorf("truncate long = %q, want %q", got, want)
	}
	if len([]rune(got)) != 10 {
		t.Errorf("truncate returned %d runes, want 10: %q", len([]rune(got)), got)
	}
}

func TestRenderGridAlignmentWithSubRows(t *testing.T) {
	rows := [][]cell{
		{hue(dim, "REPO"), hue(dim, "BRANCH"), hue(dim, "vs MAIN"), hue(dim, "PR"), hue(dim, "RV"), hue(dim, "CI")},
		{txt("caddy"), txt("main"), hue(dim, "-"), txt(""), txt(""), txt("")},
		{txt(""), hue(cyan, "└ feat/http3-probe"), hue(red, "-14!"), txt("#3232"), hue(dim, "·"),
			rawCell(hue(red, "✗").String() + " " + hue(dim, "lint +2").String())},
		{txt("grafana"), hue(cyan, "fix/nil-deref"), txt("-4"), txt("#887"), hue(green, "✓"), hue(green, "✓")},
	}

	var plain, colored bytes.Buffer
	colorOn = false
	renderGrid(&plain, rows, 2)
	colorOn = true
	renderGrid(&colored, rows, 2)
	colorOn = false

	if got := stripANSI(colored.String()); got != plain.String() {
		t.Errorf("color changed the layout:\nplain:\n%s\nstripped:\n%s", plain.String(), got)
	}
	for _, line := range strings.Split(strings.TrimRight(plain.String(), "\n"), "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("trailing whitespace on %q", line)
		}
	}
}

func TestPickQueue(t *testing.T) {
	now := time.Now()
	mk := func(repo string, n int, draft bool, ageDays int) ghSearchPR {
		var p ghSearchPR
		p.Number = n
		p.Title = fmt.Sprintf("pr %d", n)
		p.IsDraft = draft
		p.CreatedAt = now.AddDate(0, 0, -ageDays)
		p.Repository.Name = repo
		return p
	}
	list := []ghSearchPR{
		mk("Caddy", 1, false, 2),
		mk("caddy", 2, true, 9),      // draft, dropped
		mk("elsewhere", 3, false, 5), // outside the workspace, dropped
		mk("grafana", 4, false, 7),
	}
	names := map[string]string{"caddy": "caddy", "grafana": "grafana"}

	got := pickQueue(list, names)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	// Oldest first: the person kept waiting longest goes on top.
	if got[0].Number != 4 || got[1].Number != 1 {
		t.Errorf("order = #%d then #%d, want #4 then #1", got[0].Number, got[1].Number)
	}
	// Repo name matching is case-insensitive and reports the workspace's spelling.
	if got[1].Repo != "caddy" {
		t.Errorf("repo = %q, want %q", got[1].Repo, "caddy")
	}
}

func TestRepoNamesLowercasesKeys(t *testing.T) {
	m := repoNames([]Repo{{Name: "CloudScanner"}, {Name: "kite"}})
	if m["cloudscanner"] != "CloudScanner" || m["kite"] != "kite" {
		t.Errorf("repoNames = %+v", m)
	}
}

func TestCountPRErrs(t *testing.T) {
	repos := []Repo{{Name: "a", PRErr: true}, {Name: "b"}, {Name: "c", PRErr: true}}
	if got := countPRErrs(repos); got != 2 {
		t.Errorf("countPRErrs = %d, want 2", got)
	}
}

// TestAttachPRsMarksPRErrOnFailedLookup needs no gh: dir has no .git, so any
// gh call fails fast, locally, without touching the network.
func TestAttachPRsMarksPRErrOnFailedLookup(t *testing.T) {
	dir := t.TempDir()
	repos := []Repo{{Name: "broken", Path: dir}}
	attachPRs(repos)
	if !repos[0].PRErr {
		t.Error("PRErr = false, want true when the gh lookup fails")
	}
}

// TestRunQueueAndPRsHasNoDataRace drives the exact concurrent shape attachAll
// uses: the review-queue lookup running alongside attachPRs, which writes
// r.PR and r.Others on the same repos slice. It needs no gh: Path is a
// directory with no .git, so any gh call fails fast, locally, before ever
// reaching the network. Run with -race; that is what catches a regression.
func TestRunQueueAndPRsHasNoDataRace(t *testing.T) {
	dir := t.TempDir()
	repos := make([]Repo, 40)
	for i := range repos {
		repos[i] = Repo{Name: fmt.Sprintf("repo%d", i), Branch: "main", Path: dir}
	}
	names := repoNames(repos)

	queue := runQueueAndPRs(repos, names, func(map[string]string) []ReviewReq {
		return []ReviewReq{{Repo: "x"}}
	})
	if len(queue) != 1 || queue[0].Repo != "x" {
		t.Fatalf("queue = %+v, want the one entry from queueFn", queue)
	}
}

// TestFillRowsGoneUpstreamRendersUnknown is the sub-row half of a bug where a
// deleted-and-pruned upstream still renders as "level with upstream": git
// keeps %(upstream:short) populated even after the remote-tracking ref is
// pruned, so only %(upstream:track)'s [gone] marker reveals it.
func TestFillRowsGoneUpstreamRendersUnknown(t *testing.T) {
	_, clone := workspace(t)

	mustGit(t, clone, "checkout", "-q", "-b", "feature")
	writeFile(t, clone, "feature.txt", "feature")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "feature work")
	mustGit(t, clone, "push", "-q", "-u", "origin", "feature")
	mustGit(t, clone, "checkout", "-q", "main")

	mustGit(t, clone, "push", "-q", "origin", "--delete", "feature")
	mustGit(t, clone, "fetch", "-q", "--prune", "origin")

	m := refs(clone)
	feat, ok := m["feature"]
	if !ok {
		t.Fatalf("feature missing from refs: %v", m)
	}
	if !feat.Gone {
		t.Fatal("Gone = false, want true after the remote branch was deleted and pruned")
	}
	if feat.Upstream == "" {
		t.Fatal("test setup: Upstream should still read origin/feature even though it's gone")
	}

	rows := []PRRow{{Branch: "feature"}}
	fillRows(clone, "main", rows, m)
	if !rows[0].NoUpstream {
		t.Error("NoUpstream = false, want true: the upstream branch is gone")
	}
}

// TestSplitPRsKeepsBothWhenTwoPRsShareHeadBranch is the correctness bug: two
// open PRs on the same head branch (against different bases) used to have
// the second silently overwrite own, dropping the first without even a
// sub-row.
func TestSplitPRsKeepsBothWhenTwoPRsShareHeadBranch(t *testing.T) {
	list := []ghPR{
		{Number: 1, HeadRefName: "feature", ReviewDecision: "APPROVED"},
		{Number: 2, HeadRefName: "feature", ReviewDecision: "CHANGES_REQUESTED"},
	}
	own, others := splitPRs("feature", list)
	if own == nil || own.Number != 1 {
		t.Fatalf("own = %+v, want #1 (the first match)", own)
	}
	if len(others) != 1 || others[0].PR.Number != 2 {
		t.Fatalf("others = %+v, want one sub-row for #2", others)
	}
}

// TestReviewQueueArgsSortsOldestFirstExplicitly guards against gh search
// prs' best-match default ordering: without explicit --sort/--order, --limit
// truncates an arbitrary 30 results rather than the newest, which is the
// right end to lose given the oldest PR belongs on top.
func TestReviewQueueArgsSortsOldestFirstExplicitly(t *testing.T) {
	args := reviewQueueArgs()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--sort created") {
		t.Errorf("args = %v, want --sort created", args)
	}
	if !strings.Contains(joined, "--order asc") {
		t.Errorf("args = %v, want --order asc", args)
	}
	if !strings.Contains(joined, "createdAt") {
		t.Errorf("args = %v, want createdAt in --json", args)
	}
}

// TestEachPRSkipsErroredRepos: printTable's row loop skips r.Err != nil, so
// eachPR must too, or footer counts and column visibility can disagree with
// what actually renders.
func TestEachPRSkipsErroredRepos(t *testing.T) {
	repos := []Repo{
		{Name: "broken", Err: errors.New("boom"), PR: &PR{Number: 1}},
		{Name: "ok", PR: &PR{Number: 2}},
	}
	var seen []int
	eachPR(repos, func(p *PR) { seen = append(seen, p.Number) })
	if len(seen) != 1 || seen[0] != 2 {
		t.Errorf("eachPR visited %v, want only the healthy repo's PR (#2)", seen)
	}
}

// TestFillRowsUnknownMainWhenDefaultUnresolved is the fillRows half of the
// "unknown rendered as level" bug: with no default branch at all, BehindMain
// stays at its zero value, which used to render as "level with main"
// instead of "unknown".
func TestFillRowsUnknownMainWhenDefaultUnresolved(t *testing.T) {
	_, clone := workspace(t)
	m := refs(clone)

	rows := []PRRow{{Branch: "main"}}
	fillRows(clone, "", rows, m)
	if rows[0].LastCommit.IsZero() {
		t.Fatal("test setup: row never resolved, assertion would be vacuous")
	}
	if rows[0].Resolved {
		t.Error("Resolved = true with no default branch known, want false so vs MAIN renders unknown")
	}
}

// TestCollectSkipsConflictsWhenNotNeeded proves collect's withConflicts=false
// path never spawns the merge-tree subprocess: this fixture is the exact
// setup from TestConflictsDetectsAndLeavesTreeAlone, a genuine conflict, so
// Conflicts staying false can only mean the check was skipped.
func TestCollectSkipsConflictsWhenNotNeeded(t *testing.T) {
	origin, clone := workspace(t)

	addRemoteCommit(t, origin, "remote")
	mustGit(t, clone, "checkout", "-q", "-b", "feature")
	writeFile(t, clone, "one.txt", "feature version")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-qm", "feature edits one.txt")
	mustGit(t, clone, "fetch", "-q", "origin")

	tmp := t.TempDir()
	other := filepath.Join(tmp, "other")
	mustGit(t, tmp, "clone", "-q", origin, other)
	writeFile(t, other, "one.txt", "origin version")
	mustGit(t, other, "add", ".")
	mustGit(t, other, "commit", "-qm", "origin edits one.txt")
	mustGit(t, other, "push", "-q", "origin", "main")
	mustGit(t, clone, "fetch", "-q", "origin")

	withConflicts := collect(clone, true)
	if withConflicts.BehindMain == 0 {
		t.Fatal("test setup: want BehindMain > 0")
	}
	if !withConflicts.Conflicts {
		t.Fatal("test setup: want a real conflict with withConflicts=true, to prove false below means skipped")
	}

	noConflicts := collect(clone, false)
	if noConflicts.BehindMain == 0 {
		t.Fatal("test setup: want BehindMain > 0 even without the conflict check")
	}
	if noConflicts.Conflicts {
		t.Error("Conflicts = true with withConflicts=false, want the merge-tree check skipped entirely")
	}
}

// TestFillRowsSortsNewestCommitFirst is mutation-proven: flipping After to
// Before in fillRows' sort predicate left the whole suite green. Needs no
// repo on disk, since both branches fail to resolve and the sort runs on the
// LastCommit values callers set directly.
func TestFillRowsSortsNewestCommitFirst(t *testing.T) {
	rows := []PRRow{
		{Branch: "b-older", LastCommit: time.Unix(100, 0)},
		{Branch: "b-newer", LastCommit: time.Unix(200, 0)},
	}
	fillRows("", "", rows, nil)
	if rows[0].Branch != "b-newer" || rows[1].Branch != "b-older" {
		t.Errorf("order = [%s, %s], want newer commit first", rows[0].Branch, rows[1].Branch)
	}
}

// TestGhSearchPRUnmarshalsCreatedAt guards the wire-level half of item 3: the
// struct field must read gh's "createdAt" JSON key, not "updatedAt", or
// pickQueue silently sorts on the wrong signal again despite reading right in
// memory.
func TestGhSearchPRUnmarshalsCreatedAt(t *testing.T) {
	var p ghSearchPR
	if err := json.Unmarshal([]byte(`{"number":1,"createdAt":"2024-01-02T00:00:00Z"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.CreatedAt.IsZero() {
		t.Error("CreatedAt not populated from the createdAt JSON field")
	}
}

func TestPRLookupNotePluralizes(t *testing.T) {
	if got := prLookupNote(1); got != "1 PR lookup failed" {
		t.Errorf("prLookupNote(1) = %q, want %q", got, "1 PR lookup failed")
	}
	if got := prLookupNote(2); got != "2 PR lookups failed" {
		t.Errorf("prLookupNote(2) = %q, want %q", got, "2 PR lookups failed")
	}
}
