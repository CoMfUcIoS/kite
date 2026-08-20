// Command kite gives a bird-eye view of every git repo in the workspace, and
// brings each one's default branch up to date without disturbing your work.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
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

filter matches a repo name or current branch name, case-insensitively.

flags:
  --root <dir>             directory holding the repos (default: current directory)
  --no-pr                  skip the GitHub PR and CI lookups
  --version                print the version
  -h, --help               this text

the PR and CI columns appear only when there is something to put in them, so a
machine without gh installed or authenticated simply does not show them.
`

type opts struct {
	cmd    string // "status", "update" or "help"
	filter string
	root   string
	noPR   bool
}

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

	// Collect locally first so the filter can match on branch name before we
	// spend any network calls on repos we are about to drop.
	repos := filterRepos(collectAll(paths), o.filter)
	if len(repos) == 0 {
		fmt.Fprintf(os.Stderr, "kite: no repo or branch matching %q in %s\n", o.filter, root)
		os.Exit(1)
	}

	if o.cmd == "update" {
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

// version reports the module version Go stamps into the binary. Installed with
// `go install ...@v0.1.0` that is the tag; built from a working tree Go records
// no version, so fall back to the VCS revision it stamps instead. Nothing here
// needs bumping at release time, so release-please has no version file to edit.
func version() string {
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

	if len(pos) > 0 && (pos[0] == "status" || pos[0] == "update") {
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
