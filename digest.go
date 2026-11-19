package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"strings"
	"time"
)

//go:embed templates/digest.html prompt.md
var files embed.FS

// subjectPrefix is kept stable so mail filters and threads carry on.
const subjectPrefix = "GitHub digest — "

// Section is the source an email covers.
type Section struct {
	Source string
	// Events counts the activity of completely read repositories.
	Tracked, Events int
	Summary         Summary
	// Active holds the repositories with activity, listed when there is no
	// summary.
	Active       []Activity
	Repositories []RepositoryRead
	// Note stands in for a summary on a quiet day sent anyway.
	Note string
	// Error qualifies partial coverage.
	Error string
}

// RepositoryRead describes collection, independently of whether a repo was busy.
type RepositoryRead struct {
	Repo, Status, Error string
}

// GitHub links use a fixed host and treat source names as paths.
func githubURL(name string) string {
	return (&url.URL{Scheme: "https", Host: "github.com", Path: "/" + name}).String()
}

func (s Section) URL() string        { return githubURL(s.Source) }
func (r RepositoryRead) URL() string { return githubURL(r.Repo) }

func (s Section) Coverage() string {
	successful, failed, incomplete := 0, 0, 0
	for _, r := range s.Repositories {
		switch r.Status {
		case "successful":
			successful++
		case "failed":
			failed++
		case "incomplete":
			incomplete++
		}
	}
	state := "Collection complete."
	if s.Error != "" {
		state = "Collection incomplete."
	} else if s.Tracked == 0 {
		state = "No repositories to read."
	}
	return fmt.Sprintf("%s Repositories: %d successful, %d failed, %d incomplete.", state, successful, failed, incomplete)
}

// Meta qualifies activity totals by the repositories actually read.
func (s Section) Meta() string {
	if s.Tracked == 0 {
		return "Activity not checked: no repositories were resolved"
	}
	return fmt.Sprintf("%s across %s", plural(s.Events, "event"), plural(s.Tracked, "repo"))
}

// Line is one collected event, listed when there is no summary.
type Line struct{ Text, URL string }

// Lines lists a repo's events for a digest without a summary.
func (a Activity) Lines() []Line {
	var out []Line
	for _, c := range a.Commits {
		short := c.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		out = append(out, Line{fmt.Sprintf("commit %s %s (%s)", short, c.Message, c.Author), "https://github.com/" + a.Repo + "/commit/" + c.SHA})
	}
	for _, g := range []struct {
		label, path string
		items       []Item
	}{
		{"PR opened", "pull", a.PRsOpened}, {"PR merged", "pull", a.PRsMerged}, {"PR closed without merge", "pull", a.PRsClosed},
		{"issue opened", "issues", a.IssuesOpened}, {"issue closed", "issues", a.IssuesClosed},
	} {
		for _, it := range g.items {
			out = append(out, Line{fmt.Sprintf("%s #%d %s (%s)", g.label, it.Number, it.Title, it.Author), fmt.Sprintf("https://github.com/%s/%s/%d", a.Repo, g.path, it.Number)})
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

var digestTmpl = template.Must(template.ParseFS(files, "templates/digest.html"))

// render builds one source's email, HTML and plain text.
func render(label, timezone string, s Section) (html, text string, err error) {
	footer := plural(s.Events, "event")
	if s.Summary.Headline.Text != "" {
		footer += " · summarised by Claude"
	}
	footer += " · sent by gh-digest"
	var b bytes.Buffer
	if err := digestTmpl.Execute(&b, map[string]any{
		"Subject": subjectPrefix + label, "Timezone": timezone, "Sections": []Section{s}, "Footer": footer,
	}); err != nil {
		return "", "", err
	}
	return b.String(), renderText(label, timezone, s, footer), nil
}

// renderText is the plain alternative: the same content, no markup.
func renderText(label, timezone string, s Section, footer string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("%s%s", subjectPrefix, label)
	w("Activity for the listed calendar dates, in %s.", timezone)
	w("")
	w("## %s", s.Source)
	w("Source: %s", s.URL())
	w("%s", s.Coverage())
	for _, r := range s.Repositories {
		w("- %s: %s (%s)", r.Repo, r.Status, r.URL())
	}
	w("(%s)", s.Meta())
	if s.Note != "" {
		w("%s", s.Note)
	}
	if s.Summary.Headline.Text != "" {
		w("%s", claimText(s.Summary.Headline))
	}
	for _, r := range s.Summary.Repos {
		w("")
		w("%s", r.Repo)
		if r.Summary.Text != "" {
			w("%s", claimText(r.Summary))
		}
		list := func(label string, items []Claim) {
			if len(items) == 0 {
				return
			}
			w("%s:", label)
			for _, it := range items {
				w("- %s", claimText(it))
			}
		}
		list("Shipped", r.Shipped)
		list("In progress", r.InProgress)
	}
	if s.Summary.Headline.Text == "" {
		for _, a := range s.Active {
			w("")
			w("%s", a.Repo)
			for _, l := range a.Lines() {
				w("- %s (%s)", l.Text, l.URL)
			}
		}
	}
	w("")
	w("---")
	w("%s", footer)
	return b.String()
}

// claimText mirrors the HTML's adjacent evidence links in the plain alternative.
func claimText(c Claim) string {
	var b strings.Builder
	b.WriteString(c.Text)
	for _, e := range c.Links {
		fmt.Fprintf(&b, " [%s: %s]", e.Label, e.URL)
	}
	return b.String()
}

// readSource reads every repository the source resolves, each once. Failed
// and incomplete repositories are recorded on the section, not returned.
func readSource(ctx context.Context, c *client, source string, w window) (Section, error) {
	repos, err := c.resolve(ctx, source)
	if err != nil {
		return Section{}, err
	}
	s := Section{Source: source, Tracked: len(repos)}
	failed := 0
	for _, repo := range repos {
		a, err := c.activity(ctx, repo, w)
		if err != nil {
			status := "failed"
			if a.readParts > 0 {
				status = "incomplete"
			}
			s.Repositories = append(s.Repositories, RepositoryRead{Repo: repo, Status: status, Error: err.Error()})
			failed++
			continue
		}
		s.Repositories = append(s.Repositories, RepositoryRead{Repo: repo, Status: "successful"})
		if n := a.Events(); n > 0 {
			s.Events += n
			s.Active = append(s.Active, a)
		}
	}
	if failed > 0 {
		s.Error = fmt.Sprintf("%d of %d repositories could not be read; collection incomplete", failed, len(repos))
	}
	if len(s.Active) == 0 {
		if len(repos) == 0 {
			s.Note = "No repositories were resolved for this source (archived repositories are excluded); no activity was checked."
		} else {
			s.Note = fmt.Sprintf("No activity across %s that day.", plural(len(repos), "tracked repo"))
		}
	}
	return s, nil
}

// sender delivers one email.
type sender func(ctx context.Context, to []string, subject, text, html string) error

// options is one run.
type options struct {
	Sources     []string
	Date        time.Time
	Loc         *time.Location
	TZ          string
	To          []string
	AlsoTo      map[string][]string // lower-cased source → extra recipients
	SendOnEmpty bool
	HTML        bool
	Attempts    int // summary attempts per source
}

// built is one source's email, ready to print or send.
type built struct {
	Source, Subject, HTML, Text string
}

// run reads and summarises every source before sending anything, so an
// incomplete read or a rejected summary sends nothing and a rerun of the
// same date never repeats an email. stderr carries status lines only.
func run(ctx context.Context, c *client, sum summarizer, send sender, o options, stdout, stderr io.Writer) error {
	w := windowFor(o.Date, o.Loc)
	date := o.Date.Format("2006-01-02")
	var sections []Section
	var failed []string
	for _, source := range o.Sources {
		s, err := readSource(ctx, c, source, w)
		if err != nil {
			fmt.Fprintf(stderr, "%s: could not list repositories: %v\n", source, err)
			failed = append(failed, source)
			continue
		}
		if s.Error != "" {
			fmt.Fprintf(stderr, "%s: %s\n", source, s.Error)
			for _, r := range s.Repositories {
				if r.Error != "" {
					fmt.Fprintf(stderr, "  %s: %s: %s\n", r.Repo, r.Status, r.Error)
				}
			}
			failed = append(failed, source)
			continue
		}
		sections = append(sections, s)
	}
	if len(failed) > 0 {
		return fmt.Errorf("incomplete read of %s; nothing sent", strings.Join(failed, ", "))
	}

	var out []built
	for _, s := range sections {
		if s.Events == 0 && !o.SendOnEmpty {
			fmt.Fprintf(stderr, "%s: quiet (%s), nothing to send\n", s.Source, plural(s.Tracked, "repo"))
			continue
		}
		if sum != nil && len(s.Active) > 0 {
			var err error
			for attempt := 1; attempt <= o.Attempts; attempt++ {
				var raw Summary
				if raw, err = sum(ctx, s.Source, o.Date, o.TZ, s.Active, w); err == nil {
					if s.Summary, err = checkSummary(raw, s.Active); err == nil {
						break
					}
				}
				fmt.Fprintf(stderr, "%s: summary attempt %d failed: %v\n", s.Source, attempt, err)
			}
			if err != nil {
				return fmt.Errorf("summarising %s failed; nothing sent", s.Source)
			}
		}
		label := s.Source + " — " + date
		html, text, err := render(label, o.TZ, s)
		if err != nil {
			return err
		}
		out = append(out, built{Source: s.Source, Subject: subjectPrefix + label, HTML: html, Text: text})
	}

	for _, b := range out {
		if o.HTML {
			fmt.Fprintln(stdout, b.HTML)
		} else {
			fmt.Fprintln(stdout, b.Text)
		}
	}
	if len(o.To) == 0 {
		return nil
	}
	var sent []string
	for _, b := range out {
		to := recipients(o.To, o.AlsoTo[strings.ToLower(b.Source)])
		if err := send(ctx, to, b.Subject, b.Text, b.HTML); err != nil {
			if len(sent) > 0 {
				return fmt.Errorf("sending %s failed after sending %s: %w", b.Source, strings.Join(sent, ", "), err)
			}
			return fmt.Errorf("sending %s failed; nothing sent: %w", b.Source, err)
		}
		sent = append(sent, b.Source)
		fmt.Fprintf(stderr, "%s: sent to %d recipient(s)\n", b.Source, len(to))
	}
	return nil
}

// recipients is to, then a source's extras, without case-insensitive repeats.
func recipients(to, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range append(append([]string{}, to...), extra...) {
		if k := strings.ToLower(a); !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

var errUsage = errors.New("usage")
