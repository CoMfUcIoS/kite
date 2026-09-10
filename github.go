package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// 5s rather than 3s: under full parallelism the slowest observed call was
// 2.8s, and a timeout silently drops a repo's PR data, which is a wrong
// answer rather than a slow one. The calls are parallel, so the worst case
// stays 5s rather than multiplying.
const prTimeout = 5 * time.Second

// Review states, rendered in the RV column.
const (
	revApproved = "approved"
	revChanges  = "changes"
	revRequired = "required"
	revDraft    = "draft"
)

// PRRow is an open PR of yours on a branch this repo does not have checked
// out. It renders as a sub-row beneath the repo.
type PRRow struct {
	Branch     string
	Ahead      int // vs its own upstream
	Behind     int
	NoUpstream bool // no @{upstream}, the upstream is [gone], or the branch never resolved at all
	Resolved   bool // refFor found a ref for Branch AND the default branch is known, so vs MAIN is meaningful
	BehindMain int
	Conflicts  bool
	LastCommit time.Time
	PR         PR
}

type PR struct {
	Number  int
	CI      string // ciPass | ciFail | ciPending | ""
	Failing string // first failing check's name, when CI is ciFail
	Extra   int    // how many further checks failed
	Review  string // one of the rev* constants, or ""
}

type ghCheck struct {
	Status     string `json:"status"`     // CheckRun
	Conclusion string `json:"conclusion"` // CheckRun
	Name       string `json:"name"`       // CheckRun
	State      string `json:"state"`      // StatusContext
	Context    string `json:"context"`    // StatusContext
}

type ghPR struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	HeadRefName       string    `json:"headRefName"`
	ReviewDecision    string    `json:"reviewDecision"`
	IsDraft           bool      `json:"isDraft"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
}

// attachPRs looks up every open PR of yours in every repo. Unlike the earlier
// version it does not skip repos sitting on their default branch: a repo can
// be on main and still have an open PR whose branch was never cloned here.
func attachPRs(repos []Repo) {
	fan(len(repos), func(i int) {
		r := &repos[i]
		if r.Err != nil {
			return
		}
		list, ok := listPRs(r.Path)
		r.PRErr = !ok
		r.PR, r.Others = splitPRs(r.Branch, list)
		fillRows(r.Path, r.Default, r.Others, r.Refs)
		for j := range r.Others {
			o := &r.Others[j]
			if o.BehindMain > 0 {
				o.Conflicts = conflicts(r.Path, refFor(o.Branch, r.Refs), r.Default)
			}
		}
	})
}

// splitPRs puts the checked-out branch's PR on the repo's own row and every
// other open PR on a sub-row. Only the first PR matching current becomes own:
// two open PRs can share a head branch against different bases, and the
// second must become a sub-row rather than silently overwrite and vanish.
func splitPRs(current string, list []ghPR) (*PR, []PRRow) {
	var own *PR
	var others []PRRow
	for _, p := range list {
		pr := PR{
			Number: p.Number,
			CI:     rollup(p.StatusCheckRollup),
			Review: reviewState(p.ReviewDecision, p.IsDraft),
		}
		if pr.CI == ciFail {
			pr.Failing, pr.Extra = firstFailing(p.StatusCheckRollup)
		}
		if p.HeadRefName == current && own == nil {
			own = &pr
			continue
		}
		others = append(others, PRRow{Branch: p.HeadRefName, PR: pr})
	}
	return own, others
}

// ponytail: --author @me hides a PR someone else opened from your branch,
// which the old --head query did find. Dropping the filter is not an option,
// as some repos here carry fifty open PRs. Add a --head fallback for the
// current branch if it ever bites.
//
// ponytail: --limit 20 silently truncates a repo where you have more than
// twenty open PRs.

// listPRs returns your open PRs in one repo. Anything that goes wrong yields
// no PRs and ok=false: a missing gh, a stale token or a slow API must never
// cost you the rest of the table, but the caller can still count it.
func listPRs(dir string) (list []ghPR, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--author", "@me", "--state", "open", "--limit", "20",
		"--json", "number,title,headRefName,reviewDecision,isDraft,statusCheckRollup")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	if json.Unmarshal(out, &list) != nil {
		return nil, false
	}
	return list, true
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

func reviewState(decision string, draft bool) string {
	if draft {
		return revDraft
	}
	switch decision {
	case "APPROVED":
		return revApproved
	case "CHANGES_REQUESTED":
		return revChanges
	case "REVIEW_REQUIRED":
		return revRequired
	}
	return ""
}

// firstFailing names one failed check and counts the rest. Names are sorted
// so the one on display does not change between runs on API ordering alone.
func firstFailing(checks []ghCheck) (string, int) {
	var names []string
	for _, c := range checks {
		if verdict(c) != ciFail {
			continue
		}
		switch {
		case c.Name != "":
			names = append(names, c.Name)
		case c.Context != "":
			names = append(names, c.Context)
		default:
			names = append(names, "check")
		}
	}
	if len(names) == 0 {
		return "", 0
	}
	sort.Strings(names)
	return names[0], len(names) - 1
}

// --- shared gh helpers ---

func ghAvailable() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// mergedPR returns the number of a merged pull request for this branch, or 0.
func mergedPR(dir, branch string) int {
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--head", branch, "--state", "merged", "--limit", "1", "--json", "number")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return 0
	}

	var list []ghPR
	if json.Unmarshal(out, &list) != nil || len(list) == 0 {
		return 0
	}
	return list[0].Number
}

type ghSearchPR struct {
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	IsDraft    bool      `json:"isDraft"`
	CreatedAt  time.Time `json:"createdAt"`
	Repository struct {
		Name string `json:"name"`
	} `json:"repository"`
}

// ReviewReq is an open PR waiting on your review.
type ReviewReq struct {
	Repo    string
	Number  int
	Title   string
	Created time.Time
}

// repoNames maps a lowercased repo name to the workspace's own spelling. It is
// built from the filtered repos, so the filter narrows the queue too.
func repoNames(repos []Repo) map[string]string {
	m := make(map[string]string, len(repos))
	for _, r := range repos {
		m[strings.ToLower(r.Name)] = r.Name
	}
	return m
}

// pickQueue drops drafts and anything outside the workspace, then puts the
// person kept waiting longest on top.
//
// ponytail: a repo cloned into a differently-named directory will not match
// and drops out of the queue.
func pickQueue(list []ghSearchPR, names map[string]string) []ReviewReq {
	var out []ReviewReq
	for _, p := range list {
		if p.IsDraft {
			continue
		}
		name, ok := names[strings.ToLower(p.Repository.Name)]
		if !ok {
			continue
		}
		out = append(out, ReviewReq{
			Repo: name, Number: p.Number, Title: p.Title, Created: p.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// reviewQueueArgs builds the gh search invocation. --sort and --order are
// required: gh search prs defaults to best-match ordering, so without them
// --limit truncates an arbitrary 30 results rather than the newest, which is
// the right end to lose given pickQueue puts the oldest PR on top.
func reviewQueueArgs() []string {
	return []string{"search", "prs",
		"--review-requested=@me", "--state=open", "--limit", "30",
		"--sort", "created", "--order", "asc",
		"--json", "repository,number,title,createdAt,isDraft"}
}

// reviewQueue asks GitHub once for every PR awaiting your review. gh search
// cannot carry headRefName, reviewDecision or statusCheckRollup, which is why
// the per-repo lookups exist separately rather than folding into this call.
func reviewQueue(names map[string]string) []ReviewReq {
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", reviewQueueArgs()...)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var list []ghSearchPR
	if json.Unmarshal(out, &list) != nil {
		return nil
	}
	return pickQueue(list, names)
}

// attachAll runs the per-repo lookups and the review search together, so the
// search costs no wall clock on top of the lookups.
func attachAll(repos []Repo) (bool, []ReviewReq) {
	if !ghAvailable() {
		return false, nil
	}
	return true, runQueueAndPRs(repos, repoNames(repos), reviewQueue)
}

// runQueueAndPRs runs queueFn concurrently with attachPRs. names must be built
// before either starts: attachPRs writes r.PR and r.Others on repos, and
// building names from repos on the other goroutine at the same time is a data
// race (repoNames ranges over repos, copying every field including the ones
// attachPRs writes). queueFn is a seam so tests can exercise this composition
// under -race without shelling out to gh.
func runQueueAndPRs(repos []Repo, names map[string]string, queueFn func(map[string]string) []ReviewReq) []ReviewReq {
	var queue []ReviewReq
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		queue = queueFn(names)
	}()
	attachPRs(repos)
	wg.Wait()
	return queue
}
