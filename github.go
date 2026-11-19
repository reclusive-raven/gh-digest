package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// apiBase is GitHub's REST API; tests point a client at httptest instead.
const apiBase = "https://api.github.com"

// client is a small GitHub REST client: the handful of list endpoints the
// digest reads, with pagination.
type client struct {
	http  *http.Client
	base  string
	token string
	now   func() time.Time
}

func newClient(token string) *client {
	return &client{http: &http.Client{Timeout: 15 * time.Second}, base: apiBase, token: token, now: time.Now}
}

// statusError is a non-2xx answer.
type statusError struct{ Code int }

func (e *statusError) Error() string {
	return fmt.Sprintf("github: HTTP %d (remote error text omitted)", e.Code)
}

func isStatus(err error, code int) bool {
	var se *statusError
	return errors.As(err, &se) && se.Code == code
}

type rateLimitError struct{ until time.Time }

func (e *rateLimitError) Error() string {
	if e.until.IsZero() {
		return "github: rate limited"
	}
	return "github: rate limited; retry after " + e.until.UTC().Format(time.RFC3339)
}

// rateLimited reports GitHub's primary and secondary limits, with the later
// of Retry-After and the primary reset. A run-once tool fails rather than wait.
func (c *client) rateLimited(resp *http.Response, body []byte) error {
	exhausted := resp.Header.Get("X-RateLimit-Remaining") == "0"
	limited := resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden &&
		(exhausted || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(string(body)), "secondary rate limit")))
	if !limited {
		return nil
	}
	var until time.Time
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		until = c.now().Add(time.Duration(secs) * time.Second)
	}
	if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && exhausted {
		if reset := time.Unix(secs, 0).Add(time.Second); reset.After(until) {
			until = reset
		}
	}
	return &rateLimitError{until: until}
}

// get fetches one page into out and returns the URL of the next page, or "".
func (c *client) get(ctx context.Context, rawURL string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-digest")
	// Later pages come from a Link header; the token goes only to the API's
	// own host, whatever a response names.
	if base, err := url.Parse(c.base); err != nil || req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
		return "", fmt.Errorf("github: refusing to send the token to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", redact(err, c.token)
	}
	defer resp.Body.Close()
	// Error bodies are used only to recognise secondary limits, never shown.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err := c.rateLimited(resp, body); err != nil {
			return "", err
		}
		return "", &statusError{Code: resp.StatusCode}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", fmt.Errorf("github %s: %w", req.URL.Path, err)
	}
	return nextLink(resp.Header.Get("Link")), nil
}

// redact keeps a token out of an error's text.
func redact(err error, secret string) error {
	if err == nil || secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}

// nextLink is the rel="next" URL of a Link header, or "".
func nextLink(h string) string {
	for part := range strings.SplitSeq(h, ",") {
		target, params, ok := strings.Cut(part, ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(target), "<>")
	}
	return ""
}

// paginate reads a list endpoint to its end, or up to the first item for
// which stop reports true (that item is not returned).
//
// Later pages are fetched from the Link URL exactly as GitHub gives it. It
// already carries every query parameter; adding them again drops its page
// number and refetches page one forever.
func paginate[T any](ctx context.Context, c *client, path string, q url.Values, stop func(T) bool) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", "100")
	next := c.base + path + "?" + q.Encode()
	var all []T
	visited := map[string]bool{}
	seen := map[string]bool{}
	for next != "" {
		if visited[next] {
			return nil, errors.New("github: pagination repeated a page")
		}
		visited[next] = true
		var page []T
		var err error
		if next, err = c.get(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, item := range page {
			if stop != nil && stop(item) {
				return all, nil
			}
			// Updated-order lists can overlap while being paged. Keep one
			// event per stable identity, never deduplicate by title.
			if key := eventIdentity(any(item)); key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			all = append(all, item)
		}
	}
	return all, nil
}

type ghRepo struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// resolve turns a source into the repos it tracks, sorted: owner/repo is
// itself; an owner is its organisation's repos, or a user's own. Archived
// repos cannot change, so they are left out.
func (c *client) resolve(ctx context.Context, source string) ([]string, error) {
	if strings.Contains(source, "/") {
		return []string{source}, nil
	}
	repos, err := paginate[ghRepo](ctx, c, "/orgs/"+url.PathEscape(source)+"/repos", url.Values{"type": {"all"}}, nil)
	if isStatus(err, http.StatusNotFound) {
		repos, err = c.userRepos(ctx, source)
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range repos {
		key := repositoryIdentity(r.FullName)
		if r.Archived || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r.FullName)
	}
	sort.Strings(out)
	return out, nil
}

// userRepos is a user's own repos. /users/{u}/repos lists only the public
// ones, even to the user's own token, so it is joined with /user/repos,
// which lists the private ones too but also every repo of every org the
// token can see; the owner filter keeps the user's own.
func (c *client) userRepos(ctx context.Context, user string) ([]ghRepo, error) {
	public, err := paginate[ghRepo](ctx, c, "/users/"+url.PathEscape(user)+"/repos", url.Values{"type": {"owner"}}, nil)
	if err != nil {
		return nil, err
	}
	mine, err := paginate[ghRepo](ctx, c, "/user/repos", url.Values{"affiliation": {"owner,collaborator"}}, nil)
	if err != nil {
		return nil, err
	}
	var out []ghRepo
	for _, r := range append(public, mine...) {
		if strings.EqualFold(r.Owner.Login, user) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Commit is one commit on the default branch.
type Commit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Message string `json:"message"`
}

// Item is one pull request or issue.
type Item struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author"`
	State  string `json:"state"`
}

// Activity is one repo's activity in a window.
type Activity struct {
	Repo         string
	Commits      []Commit
	PRsOpened    []Item
	PRsMerged    []Item
	PRsClosed    []Item // closed without merging
	IssuesOpened []Item
	IssuesClosed []Item
	// readParts counts fully read endpoints, including empty ones. A failure
	// after one of these is incomplete rather than wholly unread.
	readParts int
}

// Events counts everything in the window; a PR opened and merged in it
// counts twice.
func (a Activity) Events() int {
	return len(a.Commits) + len(a.PRsOpened) + len(a.PRsMerged) + len(a.PRsClosed) + len(a.IssuesOpened) + len(a.IssuesClosed)
}

// window is a half-open UTC interval [Since, Until).
type window struct{ Since, Until time.Time }

func (w window) has(t *time.Time) bool {
	return t != nil && !t.Before(w.Since) && t.Before(w.Until)
}

// windowFor is a report date's calendar day in loc, as UTC instants.
func windowFor(date time.Time, loc *time.Location) window {
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)
	return window{Since: start.UTC(), Until: start.AddDate(0, 0, 1).UTC()}
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

type ghUser struct {
	Login string `json:"login"`
}

type ghCommit struct {
	SHA    string  `json:"sha"`
	Author *ghUser `json:"author"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}

type ghPull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	User      *ghUser    `json:"user"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
	// PullRequest is set on an issue that is really a pull request.
	PullRequest json.RawMessage `json:"pull_request"`
}

func login(u *ghUser) string {
	if u == nil || u.Login == "" {
		return "unknown"
	}
	return u.Login
}

func (p ghPull) item() Item {
	state := p.State
	if p.MergedAt != nil {
		state = "merged"
	}
	return Item{Number: p.Number, Title: p.Title, Author: login(p.User), State: state}
}

// activity reads one repo's commits, pull requests and issues in w.
func (c *client) activity(ctx context.Context, repo string, w window) (Activity, error) {
	a := Activity{Repo: repo}
	base := "/repos/" + repo
	commits, err := paginate[ghCommit](ctx, c, base+"/commits", url.Values{"since": {stamp(w.Since)}, "until": {stamp(w.Until)}}, nil)
	// An empty repo answers 409, not an empty list.
	if err != nil && !isStatus(err, http.StatusConflict) {
		return a, fmt.Errorf("commits: %w", err)
	}
	a.readParts++
	for _, cm := range commits {
		author := "unknown"
		switch {
		case cm.Author != nil && cm.Author.Login != "":
			author = cm.Author.Login
		case cm.Commit.Author.Name != "":
			author = cm.Commit.Author.Name
		}
		first, _, _ := strings.Cut(cm.Commit.Message, "\n")
		a.Commits = append(a.Commits, Commit{SHA: cm.SHA, Author: author, Message: strings.TrimSpace(first)})
	}

	// Newest update first, so the list can stop at the first one older than
	// the window.
	pulls, err := paginate(ctx, c, base+"/pulls", url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"desc"}},
		func(p ghPull) bool { return p.UpdatedAt != nil && p.UpdatedAt.Before(w.Since) })
	if err != nil {
		return a, fmt.Errorf("pull requests: %w", err)
	}
	a.readParts++
	for _, p := range pulls {
		if w.has(p.CreatedAt) {
			a.PRsOpened = append(a.PRsOpened, p.item())
		}
		if w.has(p.MergedAt) {
			a.PRsMerged = append(a.PRsMerged, p.item())
		} else if p.MergedAt == nil && w.has(p.ClosedAt) {
			a.PRsClosed = append(a.PRsClosed, p.item())
		}
	}

	issues, err := paginate[ghPull](ctx, c, base+"/issues", url.Values{"state": {"all"}, "since": {stamp(w.Since)},
		"sort": {"updated"}, "direction": {"desc"}}, nil)
	if err != nil {
		return a, fmt.Errorf("issues: %w", err)
	}
	a.readParts++
	for _, is := range issues {
		if len(is.PullRequest) > 0 && string(is.PullRequest) != "null" {
			continue
		}
		if w.has(is.CreatedAt) {
			a.IssuesOpened = append(a.IssuesOpened, is.item())
		}
		if w.has(is.ClosedAt) {
			a.IssuesClosed = append(a.IssuesClosed, is.item())
		}
	}
	return a, nil
}

// repositoryIdentity is GitHub's case-insensitive owner/name identity.
func repositoryIdentity(repo string) string { return strings.ToLower(repo) }

// eventIdentity identifies a row within its repository and endpoint. Equal
// display text is never an identity.
func eventIdentity(item any) string {
	switch v := item.(type) {
	case ghCommit:
		return v.SHA
	case ghPull:
		if v.Number > 0 {
			return strconv.Itoa(v.Number)
		}
	case ghRepo:
		return repositoryIdentity(v.FullName)
	}
	return ""
}
