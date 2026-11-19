package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeGitHub serves canned JSON by path and records every request URI.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []string
	routes map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.RequestURI())
		h, ok := f.routes[r.URL.Path]
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-GitHub-Api-Version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		h(w, r)
	}))
	f.json("/user", ghUser{Login: "fixture-owner"})
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) json(path string, v any) {
	f.routes[path] = func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(v) }
}

func (f *fakeGitHub) status(path string, code int) {
	f.routes[path] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func (f *fakeGitHub) client() *client {
	c := newClient("tok")
	c.base = f.srv.URL
	return c
}

func (f *fakeGitHub) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

func repo(fullName string, archived bool) map[string]any {
	owner, _, _ := strings.Cut(fullName, "/")
	return map[string]any{"full_name": fullName, "archived": archived, "owner": map[string]any{"login": owner}}
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2&per_page=100>; rel="next", <https://api.github.com/x?page=5>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/x?page=2&per_page=100" {
		t.Fatalf("next = %q", got)
	}
	if nextLink(`<https://api.github.com/x?page=1>; rel="prev"`) != "" || nextLink("") != "" {
		t.Fatal("no next link")
	}
}

// The regression the old tool shipped: re-sending the query parameters on a
// Link URL dropped its page number and refetched page one forever.
func TestPaginateFollowsTheLinkAsGiven(t *testing.T) {
	f := newFake(t)
	f.routes["/orgs/acme/repos"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]any{repo("acme/b", false)})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/repos?per_page=100&type=all&page=2>; rel="next"`, f.srv.URL))
		_ = json.NewEncoder(w).Encode([]any{repo("acme/a", false), repo("acme/old", true)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := f.client().resolve(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"acme/a", "acme/b"}) {
		t.Fatalf("repos = %v (archived repos are skipped)", got)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[1] != "/orgs/acme/repos?per_page=100&type=all&page=2" {
		t.Fatalf("requests = %v", reqs)
	}
}

// A next-page link to another host is refused, not followed with the token.
func TestPaginateNeverSendsTheTokenElsewhere(t *testing.T) {
	var leaked sync.Map
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"), true)
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	t.Cleanup(other.Close)
	f := newFake(t)
	f.routes["/orgs/acme/repos"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/repos?page=2>; rel="next"`, other.URL))
		_ = json.NewEncoder(w).Encode([]any{repo("acme/a", false)})
	}
	if _, err := f.client().resolve(context.Background(), "acme"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	leaked.Range(func(k, _ any) bool {
		t.Errorf("the other host was called with Authorization %q", k)
		return true
	})
}

func TestResolveASlugNeedsNoRequest(t *testing.T) {
	f := newFake(t)
	got, err := f.client().resolve(context.Background(), "acme/one")
	if err != nil || !slices.Equal(got, []string{"acme/one"}) || len(f.requests()) != 0 {
		t.Fatalf("%v %v %v", got, err, f.requests())
	}
}

// A user is not an org: /orgs 404s, and the user's private repos are only
// in /user/repos, which also lists other owners' repos.
func TestResolveAUserJoinsPublicAndPrivate(t *testing.T) {
	f := newFake(t)
	f.json("/users/ana/repos", []any{repo("ana/site", false)})
	f.json("/user/repos", []any{repo("ana/site", false), repo("ana/private", false), repo("someorg/theirs", false), repo("Ana/old", true)})
	got, err := f.client().resolve(context.Background(), "ana")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"ana/private", "ana/site"}) {
		t.Fatalf("repos = %v", got)
	}
}

func TestResolveOtherErrorsPropagate(t *testing.T) {
	f := newFake(t)
	f.status("/orgs/acme/repos", http.StatusForbidden)
	if _, err := f.client().resolve(context.Background(), "acme"); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("err = %v", err)
	}
}

func TestActivity(t *testing.T) {
	col, _ := time.LoadLocation("Asia/Kolkata")
	w := windowFor(time.Date(2026, 9, 24, 0, 0, 0, 0, col), col)
	f := newFake(t)
	f.routes["/repos/acme/app/commits"] = func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") != "2026-09-23T18:30:00Z" || r.URL.Query().Get("until") != "2026-09-24T18:30:00Z" {
			t.Errorf("commit window %v", r.URL.Query())
		}
		_ = json.NewEncoder(rw).Encode([]any{
			map[string]any{"sha": testSHA, "author": map[string]any{"login": "ben"}, "commit": map[string]any{"message": "fix the parser\n\nlong body"}},
			map[string]any{"author": nil, "commit": map[string]any{"message": "wip", "author": map[string]any{"name": "Ana"}}},
		})
	}
	pr := func(n int, created, updated, closed, merged string) map[string]any {
		v := map[string]any{"number": n, "title": fmt.Sprintf("pr %d", n), "state": "open", "user": map[string]any{"login": "ana"},
			"created_at": created, "updated_at": updated}
		if closed != "" {
			v["closed_at"], v["state"] = closed, "closed"
		}
		if merged != "" {
			v["merged_at"] = merged
		}
		return v
	}
	f.json("/repos/acme/app/pulls", []any{
		pr(5, "2026-09-24T02:00:00Z", "2026-09-24T10:00:00Z", "2026-09-24T10:00:00Z", "2026-09-24T10:00:00Z"), // opened and merged
		pr(4, "2026-09-20T02:00:00Z", "2026-09-24T09:00:00Z", "2026-09-24T09:00:00Z", ""),                     // closed unmerged
		pr(3, "2026-09-24T18:40:00Z", "2026-09-24T18:40:00Z", "", ""),                                         // after the window
		pr(2, "2026-09-01T00:00:00Z", "2026-09-23T18:00:00Z", "", ""),                                         // older: the list stops here
		pr(1, "2026-09-24T01:00:00Z", "2026-09-24T01:00:00Z", "", ""),                                         // never reached
	})
	f.json("/repos/acme/app/issues", []any{
		map[string]any{"number": 7, "title": "bug", "state": "open", "user": map[string]any{"login": "cara"}, "created_at": "2026-09-24T05:00:00Z", "updated_at": "2026-09-24T05:00:00Z"},
		map[string]any{"number": 6, "title": "old bug", "state": "closed", "user": map[string]any{"login": "cara"}, "created_at": "2026-08-01T05:00:00Z", "closed_at": "2026-09-24T06:00:00Z"},
		map[string]any{"number": 5, "title": "pr 5", "state": "closed", "created_at": "2026-09-24T02:00:00Z", "pull_request": map[string]any{"url": "x"}},
	})
	a, err := f.client().activity(context.Background(), "acme/app", w)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Commits) != 2 || a.Commits[0] != (Commit{SHA: testSHA, Author: "ben", Message: "fix the parser"}) || a.Commits[1] != (Commit{Author: "Ana", Message: "wip"}) {
		t.Errorf("commits %+v", a.Commits)
	}
	nums := func(items []Item) []int {
		var out []int
		for _, i := range items {
			out = append(out, i.Number)
		}
		return out
	}
	if !slices.Equal(nums(a.PRsOpened), []int{5}) || !slices.Equal(nums(a.PRsMerged), []int{5}) || !slices.Equal(nums(a.PRsClosed), []int{4}) {
		t.Errorf("prs opened %v merged %v closed %v", nums(a.PRsOpened), nums(a.PRsMerged), nums(a.PRsClosed))
	}
	if a.PRsMerged[0].State != "merged" || a.PRsClosed[0].State != "closed" {
		t.Errorf("states %+v %+v", a.PRsMerged, a.PRsClosed)
	}
	if !slices.Equal(nums(a.IssuesOpened), []int{7}) || !slices.Equal(nums(a.IssuesClosed), []int{6}) {
		t.Errorf("issues opened %v closed %v", nums(a.IssuesOpened), nums(a.IssuesClosed))
	}
	if a.Events() != 7 {
		t.Errorf("events = %d", a.Events())
	}
}

func TestActivityOfAnEmptyRepo(t *testing.T) {
	f := newFake(t)
	f.status("/repos/acme/empty/commits", http.StatusConflict)
	f.json("/repos/acme/empty/pulls", []any{})
	f.json("/repos/acme/empty/issues", []any{})
	a, err := f.client().activity(context.Background(), "acme/empty", window{Since: time.Now().Add(-time.Hour), Until: time.Now()})
	if err != nil || a.Events() != 0 {
		t.Fatalf("%+v %v", a, err)
	}
}
