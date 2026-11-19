package main

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	s := Section{Source: "example-org", Tracked: 1, Events: 3, Repositories: []RepositoryRead{{Repo: "example-org/app", Status: "successful"}}, Summary: Summary{Headline: Claim{Text: "Search <shipped>"}, Repos: []RepoSummary{
		{Repo: "example-org/app", Summary: Claim{Text: "Ana & Ben merged #412."}, Shipped: []Claim{{Text: "#412 search"}}, InProgress: []Claim{{Text: "#413 <wip>"}}},
	}}}
	html, text, err := render("example-org — 2026-09-24", "Asia/Kolkata", s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Search &lt;shipped&gt;", "Ana &amp; Ben merged #412.", "#413 &lt;wip&gt;", "3 events across 1 repo",
		"3 events · summarised by Claude · sent by gh-digest", `content="light"`, "GitHub digest — example-org — 2026-09-24"} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if strings.Contains(html, "<wip>") || strings.Contains(html, "<h1>#412") {
		t.Error("text is escaped, and a #number is never a heading")
	}
	for _, want := range []string{"GitHub digest — example-org — 2026-09-24\n", "## example-org\nSource: https://github.com/example-org\nCollection complete. Repositories: 1 successful, 0 failed, 0 incomplete.\n- example-org/app: successful (https://github.com/example-org/app)\n(3 events across 1 repo)\nSearch <shipped>\n",
		"Shipped:\n- #412 search\nIn progress:\n- #413 <wip>\n", "---\n3 events · summarised by Claude · sent by gh-digest"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q in:\n%s", want, text)
		}
	}
}

// Without a summary the digest lists every collected event with its link.
func TestRenderWithoutSummaryListsEvents(t *testing.T) {
	sha := strings.Repeat("a", 40)
	a := Activity{Repo: "example-org/app", Commits: []Commit{{SHA: sha, Author: "ana", Message: "fix <parser>"}}, PRsMerged: []Item{{Number: 7, Title: "search", Author: "ben"}}}
	s := Section{Source: "example-org", Tracked: 1, Events: 2, Active: []Activity{a}, Repositories: []RepositoryRead{{Repo: a.Repo, Status: "successful"}}}
	html, text, err := render("example-org — 2026-09-24", "UTC", s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<a href="https://github.com/example-org/app/commit/` + sha + `">commit aaaaaaa fix &lt;parser&gt; (ana)</a>`,
		`<a href="https://github.com/example-org/app/pull/7">PR merged #7 search (ben)</a>`} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if strings.Contains(html, "summarised by Claude") || !strings.Contains(text, "- PR merged #7 search (ben) (https://github.com/example-org/app/pull/7)") {
		t.Errorf("text:\n%s", text)
	}
}
