package main

import (
	"encoding/json"
	"io"
	"time"
)

// The --json shapes. Repo stays untagged: its Refs and LocalBranches are
// internal and must not leak into the output.

type jsonStatus struct {
	Repos          []jsonRepo   `json:"repos"`
	ReviewQueue    []jsonReview `json:"reviewQueue"`
	Updates        []jsonUpdate `json:"updates,omitempty"`
	PRLookupFailed int          `json:"prLookupFailed"`
	GHMissing      bool         `json:"ghMissing"`
	Timing         *jsonTiming  `json:"timing,omitempty"`
}

type jsonTiming struct {
	TotalMs  int64 `json:"totalMs"`
	GitMs    int64 `json:"gitMs"`
	GitHubMs int64 `json:"githubMs"`
}

type jsonRepo struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Where      string `json:"where"`
	Branch     string `json:"branch"`
	Default    string `json:"default"`
	Detached   bool   `json:"detached"`
	Dirty      int    `json:"dirty"`
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	NoUpstream bool   `json:"noUpstream"`
	BehindMain int    `json:"behindMain"`
	Conflicts  bool   `json:"conflicts"`
	Rewritten  bool   `json:"rewritten"`
	// Nil on a follower: the stash stack is shared, so only the leader counts it.
	Stashes    *int        `json:"stashes,omitempty"`
	LastCommit time.Time   `json:"lastCommit,omitzero"`
	FetchedAt  time.Time   `json:"fetchedAt,omitzero"`
	WorktreeOf string      `json:"worktreeOf,omitempty"`
	PR         *jsonPR     `json:"pr,omitempty"`
	OtherPRs   []jsonPRRow `json:"otherPRs,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type jsonPR struct {
	Number       int    `json:"number"`
	Review       string `json:"review"`
	CI           string `json:"ci"`
	Failing      string `json:"failing,omitempty"`
	ExtraFailing int    `json:"extraFailing,omitempty"`
}

type jsonPRRow struct {
	Branch     string    `json:"branch"`
	Ahead      int       `json:"ahead"`
	Behind     int       `json:"behind"`
	NoUpstream bool      `json:"noUpstream"`
	BehindMain int       `json:"behindMain"`
	Conflicts  bool      `json:"conflicts"`
	Resolved   bool      `json:"resolved"`
	LastCommit time.Time `json:"lastCommit,omitzero"`
	PR         jsonPR    `json:"pr"`
}

type jsonReview struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

type jsonUpdate struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Default string `json:"default"`
	Status  string `json:"status"`
	Delta   int    `json:"delta"`
	Error   string `json:"error,omitempty"`
}

type jsonStash struct {
	Repo      string    `json:"repo"`
	Ref       string    `json:"ref"`
	Age       string    `json:"age"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	Subject   string    `json:"subject"`
}

type jsonPrune struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Action  string `json:"action"`
	// Worktree is the directory --delete removes with the branch.
	Worktree string `json:"worktree,omitempty"`
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func toJSONPR(p PR) jsonPR {
	return jsonPR{Number: p.Number, Review: p.Review, CI: p.CI, Failing: p.Failing, ExtraFailing: p.Extra}
}

func statusJSON(repos []Repo, queue []ReviewReq, updates []Result, prLookupFailed int, ghMissing bool) jsonStatus {
	sortRepos(repos)
	out := jsonStatus{
		Repos:          make([]jsonRepo, 0, len(repos)),
		ReviewQueue:    make([]jsonReview, 0, len(queue)),
		PRLookupFailed: prLookupFailed,
		GHMissing:      ghMissing,
	}
	for _, r := range repos {
		j := jsonRepo{
			Name: r.Name, Path: r.Path, Where: r.Where,
			Branch: r.Branch, Default: r.Default, Detached: r.Detached,
			Dirty: r.Dirty, Ahead: r.Ahead, Behind: r.Behind, NoUpstream: r.NoUpstream,
			BehindMain: r.BehindMain, Conflicts: r.Conflicts, Rewritten: r.Rewritten,
			LastCommit: r.LastCommit, FetchedAt: r.FetchedAt,
		}
		if r.Follower {
			j.WorktreeOf = r.LeaderName
		} else {
			j.Stashes = &r.Stashes
		}
		if r.PR != nil {
			p := toJSONPR(*r.PR)
			j.PR = &p
		}
		for _, o := range r.Others {
			j.OtherPRs = append(j.OtherPRs, jsonPRRow{
				Branch: o.Branch, Ahead: o.Ahead, Behind: o.Behind, NoUpstream: o.NoUpstream,
				BehindMain: o.BehindMain, Conflicts: o.Conflicts, Resolved: o.Resolved,
				LastCommit: o.LastCommit, PR: toJSONPR(o.PR),
			})
		}
		if r.Err != nil {
			j.Error = r.Err.Error()
		}
		out.Repos = append(out.Repos, j)
	}
	for _, q := range queue {
		out.ReviewQueue = append(out.ReviewQueue, jsonReview{Repo: q.Repo, Number: q.Number, Title: q.Title, CreatedAt: q.Created})
	}
	sortResults(updates)
	for _, u := range updates {
		j := jsonUpdate{Repo: u.Repo, Branch: u.Branch, Default: u.Default, Status: u.Status, Delta: u.Delta}
		if u.Err != nil {
			j.Error = u.Err.Error()
		}
		out.Updates = append(out.Updates, j)
	}
	return out
}

func stashJSON(stashes []Stash) []jsonStash {
	sortStashes(stashes)
	out := make([]jsonStash, 0, len(stashes))
	for _, s := range stashes {
		out = append(out, jsonStash{Repo: s.Repo, Ref: s.Ref, Age: s.Age, CreatedAt: s.Created, Subject: s.Subject})
	}
	return out
}

func pruneJSON(rows []pruneRow) []jsonPrune {
	out := make([]jsonPrune, 0, len(rows))
	for _, p := range rows {
		out = append(out, jsonPrune{Repo: p.Repo, Branch: p.Name, Verdict: p.verdict(), Reason: p.reason, Action: p.action, Worktree: p.Worktree})
	}
	return out
}
