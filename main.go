// Command kite gives a bird-eye view of every git repo in the workspace, and
// brings each one's default branch up to date without disturbing your work.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const usage = `kite - bird-eye view of every repo in the workspace

usage:
  kite [filter]            status table (default command)
  kite status [filter]     same, explicit
  kite update [filter]     fetch, fast-forward every default branch, then the table

filter matches a repo name or current branch name, case-insensitively.

flags:
  --no-pr                  skip the GitHub PR and CI lookups
  -h, --help               this text

root directory is $KITE_ROOT, or ~/Apps when unset.
`

func main() {
	cmd, filter, noPR := parseArgs(os.Args[1:])
	initColor()

	root := rootDir()
	paths := discover(root)
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "kite: no git repos in %s\n", root)
		os.Exit(1)
	}

	// Collect locally first so the filter can match on branch name before we
	// spend any network calls on repos we are about to drop.
	repos := filterRepos(collectAll(paths), filter)
	if len(repos) == 0 {
		fmt.Fprintf(os.Stderr, "kite: no repo or branch matching %q in %s\n", filter, root)
		os.Exit(1)
	}

	if cmd == "update" {
		printUpdates(os.Stdout, updateAll(repos))
		fmt.Println()
		repos = collectAll(pathsOf(repos))
	}
	if !noPR {
		attachPRs(repos)
	}
	printTable(os.Stdout, repos, cmd == "update")
}

func parseArgs(args []string) (cmd, filter string, noPR bool) {
	cmd = "status"
	var pos []string
	for _, a := range args {
		switch a {
		case "--no-pr", "-no-pr":
			noPR = true
		case "-h", "--help", "help":
			fmt.Print(usage)
			os.Exit(0)
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "kite: unknown flag %s\n\n%s", a, usage)
				os.Exit(2)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) > 0 && (pos[0] == "status" || pos[0] == "update") {
		cmd, pos = pos[0], pos[1:]
	}
	if len(pos) > 0 {
		filter = pos[0]
	}
	return cmd, filter, noPR
}

func rootDir() string {
	if r := os.Getenv("KITE_ROOT"); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Apps")
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
}

func txt(s string) cell        { return cell{text: s} }
func hue(color, s string) cell { return cell{text: s, color: color} }
func (c cell) width() int      { return utf8.RuneCountInString(c.text) }
func (c cell) String() string {
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
		for last >= 0 && r[last].text == "" {
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

func printTable(w io.Writer, repos []Repo, afterUpdate bool) {
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })

	header := []string{"REPO", "BRANCH", "DIRTY", "↑↓", "vs MAIN", "STASH", "LAST", "PR", "CI"}
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
		rows = append(rows, []cell{
			txt(r.Name), r.branchCell(), r.dirtyCell(), r.upstreamCell(), r.behindMainCell(),
			r.stashCell(), hue(dim, age(r.LastCommit)), txt(r.prCell()), r.ciCell(),
		})
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
		row := []cell{glyph, txt(res.Repo), msg}
		if res.Branch != "" && res.Branch != res.Default {
			row = append(row, hue(dim, "(on "+res.Branch+")"))
		}
		rows = append(rows, row)
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
