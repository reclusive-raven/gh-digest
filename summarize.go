package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// summaryModel is the Claude model that writes the digest.
const summaryModel = "claude-sonnet-5-5"

var summarySchema = json.RawMessage(`{"type":"object","additionalProperties":false,
 "$defs":{"claim":{"type":"object","additionalProperties":false,
  "required":["text","evidence"],"properties":{
   "text":{"type":"string"},"evidence":{"type":"array","items":{"type":"string"}}}}},
 "required":["headline","repos"],
 "properties":{
  "headline":{"$ref":"#/$defs/claim"},
  "repos":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["repo","summary","shipped","in_progress"],
   "properties":{
    "repo":{"type":"string"},
    "summary":{"$ref":"#/$defs/claim"},
    "shipped":{"type":"array","items":{"$ref":"#/$defs/claim"}},
    "in_progress":{"type":"array","items":{"$ref":"#/$defs/claim"}}}}}}}`)

// Claim keeps model text beside its collected event identities. Links are
// resolved only by Go, never accepted from the model.
type Claim struct {
	Text     string     `json:"text"`
	Evidence []string   `json:"evidence"`
	Links    []Evidence `json:"-"`
}

// Evidence is a collected event's identity and trusted link. Event kinds
// distinguish opening a PR from merging it, even on the same day.
type Evidence struct {
	ID    string `json:"id"`
	Repo  string `json:"repo"`
	Label string `json:"label"`
	URL   string `json:"-"`
}

// Summary is one source's digest as Claude wrote it and Go checked it.
type Summary struct {
	Headline Claim         `json:"headline"`
	Repos    []RepoSummary `json:"repos"`
}

// RepoSummary is one repo's part of a Summary.
type RepoSummary struct {
	Repo       string  `json:"repo"`
	Summary    Claim   `json:"summary"`
	Shipped    []Claim `json:"shipped"`
	InProgress []Claim `json:"in_progress"`
}

var repoIdentity = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
var commitIdentity = regexp.MustCompile(`^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

// collectedEvidence is shared by the prompt and validator, so a reference
// cannot address an uncollected repo, event kind, number or commit.
func collectedEvidence(active []Activity) []Evidence {
	var out []Evidence
	for _, a := range active {
		if !repoIdentity.MatchString(a.Repo) || strings.HasSuffix(a.Repo, "/.") || strings.HasSuffix(a.Repo, "/..") {
			continue
		}
		add := func(kind, identity, label, path string) {
			out = append(out, Evidence{ID: a.Repo + ":" + kind + ":" + identity, Repo: a.Repo,
				Label: a.Repo + " " + label, URL: "https://github.com/" + a.Repo + "/" + path + "/" + identity})
		}
		for _, c := range a.Commits {
			if commitIdentity.MatchString(c.SHA) {
				add("commit", c.SHA, "commit "+c.SHA[:7], "commit")
			}
		}
		for _, group := range []struct {
			kind, label, path string
			items             []Item
		}{
			{"pr_opened", "PR opened", "pull", a.PRsOpened},
			{"pr_merged", "PR merged", "pull", a.PRsMerged},
			{"pr_closed", "PR closed without merge", "pull", a.PRsClosed},
			{"issue_opened", "issue opened", "issues", a.IssuesOpened},
			{"issue_closed", "issue closed", "issues", a.IssuesClosed},
		} {
			for _, item := range group.items {
				if item.Number > 0 {
					id := fmt.Sprint(item.Number)
					add(group.kind, id, group.label+" #"+id, group.path)
				}
			}
		}
	}
	return out
}

// payload includes the collected identities and a catalog of citable events.
// URLs are constructed in Go and never requested from the model.
func payload(active []Activity, w window) map[string]any {
	repos := make([]map[string]any, 0, len(active))
	for _, a := range active {
		repos = append(repos, map[string]any{
			"repo":    a.Repo,
			"commits": nonNil(a.Commits),
			"pull_requests": map[string]any{
				"opened": nonNil(a.PRsOpened), "merged": nonNil(a.PRsMerged), "closed_without_merge": nonNil(a.PRsClosed),
			},
			"issues": map[string]any{"opened": nonNil(a.IssuesOpened), "closed": nonNil(a.IssuesClosed)},
		})
	}
	return map[string]any{
		"date_range_utc": map[string]string{"since": stamp(w.Since), "until": stamp(w.Until)},
		"repositories":   repos,
		"evidence":       nonNil(collectedEvidence(active)),
	}
}

// nonNil keeps an empty list as [] rather than null in the JSON.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// summarizer writes one source's digest from its active repositories.
type summarizer func(ctx context.Context, source string, day time.Time, tz string, active []Activity, w window) (Summary, error)

// claudeSummarizer asks Claude through the Messages API, with the summary
// schema as structured output. The prompt names the day, so "yesterday"
// means the report date rather than the day the model is told it is.
func claudeSummarizer(c anthropic.Client, system string, timeout time.Duration) summarizer {
	var schema map[string]any
	if err := json.Unmarshal(summarySchema, &schema); err != nil {
		panic(err)
	}
	return func(ctx context.Context, source string, day time.Time, tz string, active []Activity, w window) (Summary, error) {
		if len(active) == 0 {
			return Summary{}, errors.New("nothing to summarise")
		}
		body, err := json.MarshalIndent(payload(active, w), "", "  ")
		if err != nil {
			return Summary{}, err
		}
		prompt := fmt.Sprintf("Summarize the GitHub activity of %s (%s), the whole calendar day in %s, across the listed "+
			"repositories of %s, following the system instructions.\n\n```json\n%s\n```",
			day.Format("Monday 2 January 2006"), day.Format("2006-01-02"), tz, source, body)
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		msg, err := c.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
			Model:     summaryModel,
			MaxTokens: 16000,
			System:    []anthropic.BetaTextBlockParam{{Text: system}},
			Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt))},
			OutputConfig: anthropic.BetaOutputConfigParam{
				Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
			},
			// A refused request is re-served by the API's default fallback model.
			Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
			Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		})
		if err != nil {
			return Summary{}, err
		}
		if msg.StopReason != anthropic.BetaStopReasonEndTurn {
			return Summary{}, fmt.Errorf("claude stopped with %q", msg.StopReason)
		}
		var text strings.Builder
		for _, block := range msg.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		var out Summary
		dec := json.NewDecoder(bytes.NewReader([]byte(text.String())))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out); err != nil {
			return Summary{}, fmt.Errorf("claude's digest is not the requested JSON: %w", err)
		}
		return out, nil
	}
}

// checkSummary refuses unsupported claims before rendering or outbox creation.
// Optional empty text is omitted; every nonempty claim needs valid evidence.
func checkSummary(s Summary, active []Activity) (Summary, error) {
	known := map[string]string{}
	for _, a := range active {
		known[strings.ToLower(a.Repo)] = a.Repo
	}
	catalog := map[string]Evidence{}
	for _, e := range collectedEvidence(active) {
		catalog[e.ID] = e
	}
	check := func(c Claim, repo string) (Claim, error) {
		text := strings.TrimSpace(c.Text)
		for _, b := range []string{"- ", "* ", "• "} {
			text = strings.TrimSpace(strings.TrimPrefix(text, b))
		}
		out := Claim{Text: text}
		seen := map[string]bool{}
		for _, id := range c.Evidence {
			e, ok := catalog[id]
			if !ok || (repo != "" && e.Repo != repo) {
				return Claim{}, errors.New("claude's digest has unknown or out-of-repository evidence")
			}
			if !seen[id] {
				seen[id] = true
				out.Evidence = append(out.Evidence, id)
				out.Links = append(out.Links, e)
			}
		}
		if text == "" {
			return Claim{}, nil
		}
		if len(out.Evidence) == 0 {
			return Claim{}, errors.New("claude's digest has a claim without evidence")
		}
		return out, nil
	}
	headline, err := check(s.Headline, "")
	if err != nil {
		return Summary{}, err
	}
	if headline.Text == "" {
		return Summary{}, errors.New("claude's digest has no headline")
	}
	out := Summary{Headline: headline}
	seen := map[string]bool{}
	for _, r := range s.Repos {
		name, ok := known[strings.ToLower(strings.TrimSpace(r.Repo))]
		if !ok || seen[name] {
			continue
		}
		rs := RepoSummary{Repo: name}
		if rs.Summary, err = check(r.Summary, name); err != nil {
			return Summary{}, err
		}
		for _, group := range []struct {
			in  []Claim
			out *[]Claim
		}{{r.Shipped, &rs.Shipped}, {r.InProgress, &rs.InProgress}} {
			for _, c := range group.in {
				checked, err := check(c, name)
				if err != nil {
					return Summary{}, err
				}
				if checked.Text != "" {
					*group.out = append(*group.out, checked)
				}
			}
		}
		if rs.Summary.Text == "" && len(rs.Shipped) == 0 && len(rs.InProgress) == 0 {
			continue
		}
		seen[name] = true
		out.Repos = append(out.Repos, rs)
	}
	if len(out.Repos) == 0 {
		return Summary{}, errors.New("claude's digest named none of the repos it was given")
	}
	return out, nil
}
