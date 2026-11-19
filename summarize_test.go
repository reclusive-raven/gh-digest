package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func evidenceFixture() []Activity {
	return []Activity{
		{Repo: "acme/app", Commits: []Commit{{SHA: testSHA, Message: "fix parser"}},
			PRsOpened: []Item{{Number: 12}}, PRsMerged: []Item{{Number: 12}}, PRsClosed: []Item{{Number: 13}},
			IssuesOpened: []Item{{Number: 14}}, IssuesClosed: []Item{{Number: 15}}},
		{Repo: "acme/web", IssuesOpened: []Item{{Number: 12}}},
	}
}

func cited(text string, ids ...string) Claim { return Claim{Text: text, Evidence: ids} }

func summaryFixture() Summary {
	return Summary{Headline: cited(" Parser shipped. ", "acme/app:pr_merged:12", "acme/app:commit:"+testSHA), Repos: []RepoSummary{
		{Repo: " Acme/App ", Summary: cited("Ana merged the parser.", "acme/app:pr_merged:12"),
			Shipped:    []Claim{cited("- #12 parser", "acme/app:pr_merged:12", "acme/app:pr_merged:12"), {Text: " "}, cited("* docs", "acme/app:commit:"+testSHA)},
			InProgress: []Claim{cited("#14 follow-up", "acme/app:issue_opened:14")}},
		{Repo: "acme/app", Summary: Claim{Text: "duplicate ignored"}},
		{Repo: "someone/else", Summary: Claim{Text: "unknown repo ignored"}},
		{Repo: "acme/web", Summary: Claim{Text: " "}},
	}}
}

func TestCheckSummary(t *testing.T) {
	s := summaryFixture()
	// A link field, even if set internally, must never survive validation.
	s.Headline.Links = []Evidence{{URL: "https://example.test/forged"}}
	got, err := checkSummary(s, evidenceFixture())
	if err != nil {
		t.Fatal(err)
	}
	if got.Headline.Text != "Parser shipped." || len(got.Repos) != 1 || len(got.Headline.Links) != 2 {
		t.Fatalf("%+v", got)
	}
	r := got.Repos[0]
	if r.Repo != "acme/app" || len(r.Shipped) != 2 || r.Shipped[0].Text != "#12 parser" || r.Shipped[1].Text != "docs" ||
		!slices.Equal(r.Shipped[0].Evidence, []string{"acme/app:pr_merged:12"}) || len(r.Shipped[0].Links) != 1 {
		t.Fatalf("%+v", r)
	}
	if got.Headline.Links[0].URL != "https://github.com/acme/app/pull/12" || got.Headline.Links[1].URL != "https://github.com/acme/app/commit/"+testSHA {
		t.Fatalf("%+v", got.Headline.Links)
	}
}

func TestCheckSummaryRejectsUnsupportedClaims(t *testing.T) {
	for _, field := range []string{"headline", "summary", "shipped", "in_progress"} {
		for _, ref := range []string{"", "acme/app:pr_merged:999", "acme/app:pr_merged:14", "acme/app:issue_closed:14", "acme/app:commit:0123456", "acme/missing:pr_merged:12", "https://example.test/forged"} {
			t.Run(field+"/"+ref, func(t *testing.T) {
				s := summaryFixture()
				c := cited("unsupported")
				if ref != "" {
					c.Evidence = []string{"acme/app:pr_merged:12", ref}
				}
				switch field {
				case "headline":
					s.Headline = c
				case "summary":
					s.Repos[0].Summary = c
				case "shipped":
					s.Repos[0].Shipped = []Claim{c}
				case "in_progress":
					s.Repos[0].InProgress = []Claim{c}
				}
				if _, err := checkSummary(s, evidenceFixture()); err == nil {
					t.Fatal("unsupported claim accepted")
				}
			})
		}
	}
	for _, field := range []string{"summary", "shipped", "in_progress"} {
		t.Run("cross-repo/"+field, func(t *testing.T) {
			s := summaryFixture()
			c := cited("wrong repo", "acme/web:issue_opened:12")
			switch field {
			case "summary":
				s.Repos[0].Summary = c
			case "shipped":
				s.Repos[0].Shipped = []Claim{c}
			case "in_progress":
				s.Repos[0].InProgress = []Claim{c}
			}
			if _, err := checkSummary(s, evidenceFixture()); err == nil {
				t.Fatal("cross-repo citation accepted")
			}
		})
	}
	s := summaryFixture()
	s.Headline = Claim{}
	if _, err := checkSummary(s, evidenceFixture()); err == nil {
		t.Fatal("missing headline accepted")
	}
	s = summaryFixture()
	s.Repos = []RepoSummary{{Repo: "unknown/repo"}}
	if _, err := checkSummary(s, evidenceFixture()); err == nil {
		t.Fatal("unknown repositories accepted")
	}
}

func TestPayloadEvidenceMatchesCollectedEvents(t *testing.T) {
	active := evidenceFixture()
	body, err := json.Marshal(payload(active, window{}))
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Evidence []Evidence `json:"evidence"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"acme/app:commit:" + testSHA: "https://github.com/acme/app/commit/" + testSHA,
		"acme/app:pr_opened:12":      "https://github.com/acme/app/pull/12",
		"acme/app:pr_merged:12":      "https://github.com/acme/app/pull/12",
		"acme/app:pr_closed:13":      "https://github.com/acme/app/pull/13",
		"acme/app:issue_opened:14":   "https://github.com/acme/app/issues/14",
		"acme/app:issue_closed:15":   "https://github.com/acme/app/issues/15",
		"acme/web:issue_opened:12":   "https://github.com/acme/web/issues/12",
	}
	if len(input.Evidence) != len(want) {
		t.Fatalf("catalog: %+v", input.Evidence)
	}
	for _, e := range input.Evidence {
		s := Summary{Headline: cited("Activity", e.ID), Repos: []RepoSummary{{Repo: e.Repo, Summary: cited("Activity", e.ID)}}}
		got, err := checkSummary(s, active)
		if err != nil {
			t.Fatal(err)
		}
		if url := got.Repos[0].Summary.Links[0].URL; url != want[e.ID] {
			t.Fatalf("%s: %s", e.ID, url)
		}
		if e.URL != "" {
			t.Fatal("prompt contains a URL")
		}
	}
	// Malformed identities cannot create a citation or escape the GitHub origin.
	bad := []Activity{{Repo: "acme/../evil", Commits: []Commit{{SHA: testSHA}}}, {Repo: "acme/..", PRsOpened: []Item{{Number: 1}}},
		{Repo: "acme/app", Commits: []Commit{{SHA: ""}, {SHA: "../../../evil"}}, PRsOpened: []Item{{Number: -1}}}}
	if got := collectedEvidence(bad); len(got) != 0 {
		t.Fatalf("unsafe identities: %+v", got)
	}
}

func TestRenderEvidenceLinks(t *testing.T) {
	s := summaryFixture()
	s.Headline.Text = "Search <shipped> & ready"
	s.Repos[0].Shipped = []Claim{cited("#13 closed", "acme/app:pr_closed:13")}
	s.Repos[0].InProgress = []Claim{cited("#12 opened", "acme/app:pr_opened:12"), cited("#15 closed", "acme/app:issue_closed:15")}
	checked, err := checkSummary(s, evidenceFixture())
	if err != nil {
		t.Fatal(err)
	}
	html, text, err := render("acme — 2026-09-24", "Asia/Kolkata", Section{Source: "acme", Summary: checked})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Claim{checked.Headline, checked.Repos[0].Summary, checked.Repos[0].Shipped[0], checked.Repos[0].InProgress[0], checked.Repos[0].InProgress[1]} {
		for _, e := range c.Links {
			if !strings.Contains(html, fmt.Sprintf(`<a href="%s">[%s]</a>`, e.URL, e.Label)) || !strings.Contains(text, "["+e.Label+": "+e.URL+"]") {
				t.Fatalf("missing link: %+v\n%s\n%s", e, html, text)
			}
		}
	}
	if !strings.Contains(html, "Search &lt;shipped&gt; &amp; ready") || strings.Contains(html, "<shipped>") {
		t.Fatal("claim text is not escaped")
	}
}

func TestLegacySummaryCannotInventClaimEvidence(t *testing.T) {
	legacy := []byte(`{"headline":"Parser shipped.","repos":[{"repo":"acme/app","summary":"Parser fixed.","shipped":["#12 merged"],"in_progress":[]}]}`)
	var s Summary
	if err := json.Unmarshal(legacy, &s); err == nil {
		t.Fatal("legacy summary was silently accepted without evidence")
	}
	// A rebuilt response follows the new schema and resolves collected evidence.
	rebuilt, err := json.Marshal(summaryFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rebuilt, &s); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSummary(s, evidenceFixture()); err != nil {
		t.Fatal(err)
	}
}
