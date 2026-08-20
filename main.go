// Command kite gives a bird-eye view of every git repo in the workspace, and
// brings each one's default branch up to date without disturbing your work.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const usage = `kite - bird-eye view of every repo in a directory

usage:
  kite [filter]            status table (default command)
  kite status [filter]     same, explicit
  kite update [filter]     fetch, fast-forward every default branch, then the table
  kite prune [filter]      list finished local branches; --delete removes them
  kite stash [filter]      every stash across every repo, with age and subject
  kite path [filter]       print one repo path, for: cd $(kite path api)

filter matches a repo name or current branch name, case-insensitively. The
exception is path, which matches repo names only so it needs no git calls, and
which fails rather than print an ambiguous list a filter cannot narrow to one.

flags:
  --root <dir>             directory holding the repos (default: current directory)
  --no-pr                  skip the GitHub lookups
  --delete                 prune only: actually delete the branches
  --force                  prune only: also delete branches whose merge is unconfirmed
  --version                print the version
  -h, --help               this text

the PR and CI columns appear only when there is something to put in them, so a
machine without gh installed or authenticated simply does not show them.
`

type opts struct {
	cmd    string // status, update, prune, stash, path, version or help
	filter string
	root   string
	noPR   bool
	delete bool
	force  bool
}

var commands = []string{"status", "update", "prune", "stash", "path"}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "kite: %v\n\n%s", err, usage)
		os.Exit(2)
	}
	switch o.cmd {
	case "help":
		fmt.Print(usage)
		return
	case "version":
		fmt.Println("kite", version())
		return
	}
	requireGit()
	initColor()

	root := rootDir(o.root)
	paths := discover(root)
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "kite: no git repos in %s\n", root)
		fmt.Fprintf(os.Stderr, "      kite lists the repos inside a directory; try --root <dir>\n")
		os.Exit(1)
	}

	// path answers from directory names alone, so it stays instant. Everything
	// else needs the per-repo git calls.
	if o.cmd == "path" {
		matches := repoPaths(paths, o.filter)
		switch {
		case len(matches) == 0:
			fmt.Fprintf(os.Stderr, "kite: no repo name matching %q in %s\n", o.filter, root)
			os.Exit(1)
		case len(matches) > 1 && o.filter != "":
			// An ambiguous filter would hand `cd` several arguments, and the
			// resulting shell error says nothing useful. Fail clearly instead.
			fmt.Fprintf(os.Stderr, "kite: %d repos match %q, be more specific:\n", len(matches), o.filter)
			for _, p := range matches {
				fmt.Fprintf(os.Stderr, "        %s\n", filepath.Base(p))
			}
			os.Exit(1)
		}
		for _, p := range matches {
			fmt.Println(p)
		}
		return
	}

	// Collect locally first so the filter can match on branch name before we
	// spend any network calls on repos we are about to drop.
	repos := filterRepos(collectAll(paths), o.filter)
	if len(repos) == 0 {
		fmt.Fprintf(os.Stderr, "kite: no repo or branch matching %q in %s\n", o.filter, root)
		os.Exit(1)
	}

	switch o.cmd {
	case "prune":
		printPrune(os.Stdout, staleBranchesAll(repos, !o.noPR), o.delete, o.force)
		return
	case "stash":
		printStashes(os.Stdout, stashesAll(repos))
		return
	case "update":
		printUpdates(os.Stdout, updateAll(repos))
		fmt.Println()
		repos = collectAll(pathsOf(repos))
	}

	note := ""
	if !o.noPR && !attachPRs(repos) {
		note = "gh not installed, PR columns hidden"
	}
	printTable(os.Stdout, repos, o.cmd == "update", note)
}

// repoPaths filters discovered paths by directory name only.
func repoPaths(paths []string, filter string) []string {
	if filter == "" {
		return paths
	}
	f := strings.ToLower(filter)
	var out []string
	for _, p := range paths {
		if strings.Contains(strings.ToLower(filepath.Base(p)), f) {
			out = append(out, p)
		}
	}
	return out
}

// buildVersion is set with -ldflags "-X main.buildVersion=v1.2.3" by the
// release workflow. A binary cross-compiled from a checkout carries no module
// version, so without this stamp a released build would report its revision
// instead of its tag, and the installer compares tags.
var buildVersion string

// version reports the module version Go stamps into the binary. Installed with
// `go install ...@v0.1.0` that is the tag; built from a working tree Go records
// no version, so fall back to the VCS revision it stamps instead. Nothing here
// needs bumping at release time, so release-please has no version file to edit.
func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
			if len(rev) > 7 {
				rev = rev[:7]
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "devel"
	}
	return "devel+" + rev + dirty
}

// requireGit stops before doing anything if git is missing. Every single thing
// kite reports comes from shelling out to git, so there is no degraded mode.
func requireGit() {
	if _, err := exec.LookPath("git"); err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "kite: git is not installed, or is not on your PATH.")
	fmt.Fprintln(os.Stderr, "      kite reads everything from git, so install it and try again:")
	fmt.Fprintln(os.Stderr, "        macOS         xcode-select --install   (or: brew install git)")
	fmt.Fprintln(os.Stderr, "        debian/ubuntu sudo apt install git")
	fmt.Fprintln(os.Stderr, "        fedora        sudo dnf install git")
	os.Exit(1)
}

func parseArgs(args []string) (opts, error) {
	o := opts{cmd: "status"}
	var pos []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-pr", a == "-no-pr":
			o.noPR = true
		case a == "--delete", a == "-delete":
			o.delete = true
		case a == "--force", a == "-force":
			o.force = true
		case a == "-h", a == "--help", a == "help":
			return opts{cmd: "help"}, nil
		case a == "--version", a == "-version", a == "version":
			return opts{cmd: "version"}, nil
		case a == "--root", a == "-root":
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s needs a directory", a)
			}
			i++
			o.root = args[i]
		case strings.HasPrefix(a, "--root="), strings.HasPrefix(a, "-root="):
			o.root = a[strings.IndexByte(a, '=')+1:]
			if o.root == "" {
				return o, fmt.Errorf("--root needs a directory")
			}
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}

	if len(pos) > 0 && slices.Contains(commands, pos[0]) {
		o.cmd, pos = pos[0], pos[1:]
	}
	if len(pos) > 0 {
		o.filter = pos[0]
	}
	return o, nil
}

// rootDir defaults to the directory kite was run from.
func rootDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func filterRepos(repos []Repo, filter string) []Repo {
	if filter == "" {
		return repos
	}
	f := strings.ToLower(filter)
	var out []Repo
	for _, r := range repos {
		if strings.Contains(strings.ToLower(r.Name), f) || strings.Contains(strings.ToLower(r.Branch), f) {
			out = append(out, r)
		}
	}
	return out
}

func pathsOf(repos []Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Path
	}
	return out
}

// --- grid ---

const (
	reset  = "\033[0m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

var colorOn bool

func initColor() {
	if os.Getenv("NO_COLOR") != "" {
		return
	}
	fi, err := os.Stdout.Stat()
	colorOn = err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// cell keeps text and color apart so column widths can be measured on the text
// alone. text/tabwriter cannot do this: it counts the ANSI bytes as width and
// misaligns every colored column, whether or not StripEscape is set.
type cell struct {
	text  string
	color string
	// raw is printed verbatim and measured as zero width. Only safe as a row's
	// final cell, which is never padded. It exists so a trailing cell can mix
	// two colors, which text and color alone cannot express.
	raw string
}

func txt(s string) cell        { return cell{text: s} }
func hue(color, s string) cell { return cell{text: s, color: color} }
func rawCell(s string) cell    { return cell{raw: s} }

func (c cell) width() int {
	if c.raw != "" {
		return 0
	}
	return utf8.RuneCountInString(c.text)
}

func (c cell) String() string {
	if c.raw != "" {
		return c.raw
	}
	if c.color == "" || !colorOn || c.text == "" {
		return c.text
	}
	return c.color + c.text + reset
}

// renderGrid pads columns to the widest plain text in each, and never pads
// after a row's last cell, so no line carries trailing whitespace.
func renderGrid(w io.Writer, rows [][]cell, gap int) {
	widest := 0
	for _, r := range rows {
		if len(r) > widest {
			widest = len(r)
		}
	}
	widths := make([]int, widest)
	for _, r := range rows {
		for i, c := range r {
			if c.width() > widths[i] {
				widths[i] = c.width()
			}
		}
	}
	sep := strings.Repeat(" ", gap)
	for _, r := range rows {
		// Stop at the last cell that has text, so a row ending in blanks does
		// not pad out to the full grid width.
		last := len(r) - 1
		for last >= 0 && r[last].text == "" && r[last].raw == "" {
			last--
		}
		var b strings.Builder
		for i := 0; i <= last; i++ {
			b.WriteString(r[i].String())
			if i < last {
				b.WriteString(strings.Repeat(" ", widths[i]-r[i].width()))
				b.WriteString(sep)
			}
		}
		fmt.Fprintln(w, b.String())
	}
}

func printTable(w io.Writer, repos []Repo, afterUpdate bool, note string) {
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })

	// A column nobody can fill is a column nobody should read. This covers a
	// missing gh, an unauthenticated gh, --no-pr, and simply having no open PRs.
	showPR, showCI := false, false
	for _, r := range repos {
		if r.PR == nil {
			continue
		}
		showPR = true
		if r.PR.CI != "" {
			showCI = true
		}
	}

	header := []string{"REPO", "BRANCH", "DIRTY", "↑↓", "vs MAIN", "STASH", "LAST"}
	if showPR {
		header = append(header, "PR")
	}
	if showCI {
		header = append(header, "CI")
	}
	rows := [][]cell{{}}
	for _, h := range header {
		rows[0] = append(rows[0], hue(dim, h))
	}

	dirty, stashes := 0, 0
	var oldestFetch time.Time
	var broken []Repo

	for _, r := range repos {
		if r.Err != nil {
			broken = append(broken, r)
			continue
		}
		if r.Dirty > 0 {
			dirty++
		}
		stashes += r.Stashes
		if !r.FetchedAt.IsZero() && (oldestFetch.IsZero() || r.FetchedAt.Before(oldestFetch)) {
			oldestFetch = r.FetchedAt
		}
		row := []cell{
			txt(r.Name), r.branchCell(), r.dirtyCell(), r.upstreamCell(), r.behindMainCell(),
			r.stashCell(), hue(dim, age(r.LastCommit)),
		}
		if showPR {
			row = append(row, txt(r.prCell()))
		}
		if showCI {
			row = append(row, r.ciCell())
		}
		rows = append(rows, row)
	}
	renderGrid(w, rows, 2)

	// Errors go below the grid rather than inside it: one long git message must
	// not stretch the BRANCH column for every healthy repo.
	for _, r := range broken {
		fmt.Fprintf(w, "%s %s %s\n", hue(red, "✗"), r.Name, hue(red, firstLine(r.Err.Error())))
	}

	parts := []string{plural(len(repos), "repo")}
	if dirty > 0 {
		parts = append(parts, fmt.Sprintf("%d dirty", dirty))
	}
	if stashes > 0 {
		parts = append(parts, plural(stashes, "stash"))
	}
	if len(broken) > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable", len(broken)))
	}
	if !oldestFetch.IsZero() {
		s := "oldest fetch " + age(oldestFetch) + " ago"
		if age(oldestFetch) == "now" {
			s = "fetched just now"
		}
		if !afterUpdate {
			s += " (kite update)"
		}
		parts = append(parts, s)
	}
	if note != "" {
		parts = append(parts, note)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, hue(dim, strings.Join(parts, " · ")))
}

func (r Repo) branchCell() cell {
	switch {
	case r.Detached:
		return hue(yellow, "detached@"+r.Branch)
	case r.Branch == r.Default:
		return txt(r.Branch)
	}
	return hue(cyan, r.Branch)
}

func (r Repo) dirtyCell() cell {
	if r.Dirty == 0 {
		return hue(dim, "-")
	}
	return hue(yellow, fmt.Sprint(r.Dirty))
}

func (r Repo) upstreamCell() cell {
	if r.NoUpstream {
		return hue(dim, "·")
	}
	switch {
	case r.Ahead > 0 && r.Behind > 0:
		return hue(yellow, fmt.Sprintf("↑%d↓%d", r.Ahead, r.Behind))
	case r.Ahead > 0:
		return hue(cyan, fmt.Sprintf("↑%d", r.Ahead))
	case r.Behind > 0:
		return hue(yellow, fmt.Sprintf("↓%d", r.Behind))
	}
	return hue(dim, "-")
}

func (r Repo) behindMainCell() cell {
	if r.BehindMain == 0 {
		return hue(dim, "-")
	}
	s := fmt.Sprintf("-%d", r.BehindMain)
	if r.BehindMain >= 20 {
		return hue(yellow, s)
	}
	return txt(s)
}

func (r Repo) stashCell() cell {
	if r.Stashes == 0 {
		return hue(dim, "-")
	}
	return hue(yellow, fmt.Sprint(r.Stashes))
}

func (r Repo) prCell() string {
	if r.PR == nil {
		return ""
	}
	return fmt.Sprintf("#%d", r.PR.Number)
}

func (r Repo) ciCell() cell {
	if r.PR == nil {
		return txt("")
	}
	switch r.PR.CI {
	case ciPass:
		return hue(green, "✓")
	case ciFail:
		return hue(red, "✗")
	case ciPending:
		return hue(yellow, "○")
	}
	return txt("")
}

func printUpdates(w io.Writer, results []Result) {
	sort.Slice(results, func(i, j int) bool { return results[i].Repo < results[j].Repo })

	var rows [][]cell
	for _, res := range results {
		var glyph, msg cell
		switch res.Status {
		case statusAdvanced:
			glyph, msg = hue(green, "✓"), txt(fmt.Sprintf("%s +%d", res.Default, res.Delta))
		case statusCreated:
			glyph, msg = hue(green, "✓"), txt(res.Default+" created")
		case statusCurrent:
			glyph, msg = hue(dim, "·"), hue(dim, "up to date")
		case statusDiverged:
			glyph, msg = hue(yellow, "!"), hue(yellow, res.Default+" diverged from origin/"+res.Default+", skipped")
		case statusBlocked:
			glyph, msg = hue(yellow, "!"), hue(yellow, "local changes block fast-forward, skipped")
		default:
			glyph, msg = hue(red, "✗"), hue(red, firstLine(res.Err.Error()))
		}
		tail := msg.String()
		if res.Branch != "" && res.Branch != res.Default {
			tail += " " + hue(dim, "(on "+res.Branch+")").String()
		}
		rows = append(rows, []cell{glyph, txt(res.Repo), rawCell(tail)})
	}
	renderGrid(w, rows, 2)
}

func printPrune(w io.Writer, branches []Branch, doDelete, force bool) {
	sort.Slice(branches, func(i, j int) bool {
		if branches[i].Repo != branches[j].Repo {
			return branches[i].Repo < branches[j].Repo
		}
		return branches[i].Name < branches[j].Name
	})

	if len(branches) == 0 {
		fmt.Fprintln(w, hue(dim, "No finished branches. Nothing to prune.").String())
		return
	}

	var rows [][]cell
	deleted, wouldDelete, blocked, skipped, failed := 0, 0, 0, 0, 0

	for _, b := range branches {
		if b.Err != nil {
			rows = append(rows, []cell{txt(b.Repo), hue(red, "-"),
				rawCell(hue(red, "error: "+firstLine(b.Err.Error())).String())})
			failed++
			continue
		}

		var reason, action cell
		switch b.verdict() {
		case pruneMerged:
			reason = hue(dim, "merged into "+b.Default)
		case prunePR:
			reason = hue(dim, fmt.Sprintf("PR #%d merged", b.PRNumber))
		case pruneCurrent:
			reason = hue(dim, "checked out here")
		default:
			reason = hue(yellow, "upstream gone, merge unconfirmed")
		}

		switch {
		case b.verdict() == pruneCurrent:
			action = hue(dim, "skipped")
			skipped++
		case !b.deletable(force):
			action = hue(yellow, "needs --force")
			blocked++
		case !doDelete:
			action = hue(cyan, "would delete")
			wouldDelete++
		default:
			if err := deleteBranch(b); err != nil {
				action = hue(red, "failed: "+firstLine(err.Error()))
				failed++
			} else {
				action = hue(green, "deleted")
				deleted++
			}
		}

		rows = append(rows, []cell{txt(b.Repo), hue(cyan, b.Name), reason, action})
	}
	renderGrid(w, rows, 2)

	var parts []string
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", deleted))
	}
	if wouldDelete > 0 {
		parts = append(parts, fmt.Sprintf("%d would delete", wouldDelete))
	}
	if blocked > 0 {
		verb := "need"
		if blocked == 1 {
			verb = "needs"
		}
		parts = append(parts, fmt.Sprintf("%d %s --force", blocked, verb))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d checked out", skipped))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if !doDelete && wouldDelete > 0 {
		parts = append(parts, "nothing changed (kite prune --delete)")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, hue(dim, strings.Join(parts, " · ")).String())
}

func printStashes(w io.Writer, stashes []Stash) {
	if len(stashes) == 0 {
		fmt.Fprintln(w, hue(dim, "No stashes anywhere.").String())
		return
	}
	sort.Slice(stashes, func(i, j int) bool {
		if stashes[i].Repo != stashes[j].Repo {
			return stashes[i].Repo < stashes[j].Repo
		}
		return stashes[i].Ref < stashes[j].Ref
	})

	rows := [][]cell{{hue(dim, "REPO"), hue(dim, "STASH"), hue(dim, "AGE"), hue(dim, "SUBJECT")}}
	for _, s := range stashes {
		rows = append(rows, []cell{
			txt(s.Repo), hue(cyan, s.Ref), hue(dim, s.Age), txt(s.Subject),
		})
	}
	renderGrid(w, rows, 2)

	fmt.Fprintln(w)
	fmt.Fprintln(w, hue(dim, plural(len(stashes), "stash")+" · git -C <repo> stash show -p <ref>").String())
}

// --- small helpers ---

func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/24/7))
	}
}

func plural(n int, word string) string {
	switch {
	case n == 1:
		return fmt.Sprintf("%d %s", n, word)
	case strings.HasSuffix(word, "sh"):
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
