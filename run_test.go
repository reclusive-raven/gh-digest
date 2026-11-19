package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeOrgs serves two orgs: busy has one commit on the day, calm has none.
func fakeOrgs(t *testing.T) *fakeGitHub {
	f := newFake(t)
	f.json("/orgs/busy/repos", []any{repo("busy/app", false)})
	f.json("/orgs/calm/repos", []any{repo("calm/lib", false)})
	f.json("/repos/busy/app/commits", []any{map[string]any{"sha": testSHA, "author": map[string]any{"login": "ana"}, "commit": map[string]any{"message": "fix"}}})
	for _, p := range []string{"/repos/busy/app/pulls", "/repos/busy/app/issues", "/repos/calm/lib/commits", "/repos/calm/lib/pulls", "/repos/calm/lib/issues"} {
		f.json(p, []any{})
	}
	return f
}

type sent struct {
	to      []string
	subject string
}

func recorder(log *[]sent) sender {
	return func(_ context.Context, to []string, subject, _, _ string) error {
		*log = append(*log, sent{to, subject})
		return nil
	}
}

func testOptions(sources ...string) options {
	return options{Sources: sources, Date: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), Loc: time.UTC, TZ: "UTC",
		To: []string{"me@example.com"}, AlsoTo: map[string][]string{"busy": {"them@example.com", "ME@example.com"}}, Attempts: 3}
}

func TestRunSendsActiveSourcesOnly(t *testing.T) {
	f := fakeOrgs(t)
	var log []sent
	var out, errs bytes.Buffer
	if err := run(context.Background(), f.client(), nil, recorder(&log), testOptions("busy", "calm"), &out, &errs); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].subject != "GitHub digest — busy — 2026-09-24" || !slices.Equal(log[0].to, []string{"me@example.com", "them@example.com"}) {
		t.Fatalf("sent %+v", log)
	}
	if !strings.Contains(errs.String(), "calm: quiet") || strings.Contains(errs.String(), "fix") {
		t.Fatalf("stderr carries status only:\n%s", errs.String())
	}
	if !strings.Contains(out.String(), "commit 0123456 fix (ana)") {
		t.Fatalf("stdout:\n%s", out.String())
	}
}

// One unreadable repository anywhere means nothing is sent for any source.
func TestRunSendsNothingOnAnIncompleteRead(t *testing.T) {
	f := fakeOrgs(t)
	f.status("/repos/calm/lib/issues", http.StatusBadGateway)
	var log []sent
	var out, errs bytes.Buffer
	err := run(context.Background(), f.client(), nil, recorder(&log), testOptions("busy", "calm"), &out, &errs)
	if err == nil || len(log) != 0 || out.Len() != 0 {
		t.Fatalf("err %v, sent %+v, stdout %q", err, log, out.String())
	}
	if !strings.Contains(errs.String(), "calm/lib: incomplete") {
		t.Fatalf("stderr:\n%s", errs.String())
	}
}

// A summary rejected by the evidence check is retried, then fails the run.
func TestRunRetriesThenFailsARejectedSummary(t *testing.T) {
	f := fakeOrgs(t)
	calls := 0
	bad := func(context.Context, string, time.Time, string, []Activity, window) (Summary, error) {
		calls++
		return Summary{Headline: cited("made up", "busy/app:pr_merged:99"), Repos: []RepoSummary{{Repo: "busy/app", Summary: cited("x", "busy/app:commit:"+testSHA)}}}, nil
	}
	var log []sent
	var out, errs bytes.Buffer
	if err := run(context.Background(), f.client(), bad, recorder(&log), testOptions("busy"), &out, &errs); err == nil || calls != 3 || len(log) != 0 {
		t.Fatalf("err %v, calls %d, sent %+v", err, calls, log)
	}

	good := func(_ context.Context, _ string, _ time.Time, _ string, active []Activity, _ window) (Summary, error) {
		calls++
		if calls == 4 {
			return Summary{}, errors.New("overloaded")
		}
		return Summary{Headline: cited("Ana fixed it.", "busy/app:commit:"+testSHA), Repos: []RepoSummary{{Repo: active[0].Repo, Summary: cited("fix", "busy/app:commit:"+testSHA)}}}, nil
	}
	if err := run(context.Background(), f.client(), good, recorder(&log), testOptions("busy"), &out, &errs); err != nil || len(log) != 1 {
		t.Fatalf("err %v, sent %+v", err, log)
	}
	if !strings.Contains(out.String(), "Ana fixed it.") {
		t.Fatalf("stdout:\n%s", out.String())
	}
}

func TestCLIValidatesRecipients(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"GH_TOKEN": "tok", "SMTP_USER": "me@example.com", "SMTP_PASSWORD": "x"}[k]
	}
	for _, args := range [][]string{
		{"--to", "Me <me@example.com>", "busy"},
		{"--to", "me@example.com\r\nBcc: x@example.com", "busy"},
		{"--to", "me@example.com", "--also-to", "other=x@example.com", "busy"},
		{"--also-to", "busy=x@example.com", "busy"},
		{"--date", "24-09-2026", "busy"},
	} {
		if err := cli(context.Background(), args, env, new(bytes.Buffer), new(bytes.Buffer)); err == nil {
			t.Errorf("%q was accepted", args)
		}
	}
}

func TestBuildMessage(t *testing.T) {
	raw, err := buildMessage("me@example.com", []string{"a@example.com", "b@example.com"}, "GitHub digest — x", "plain", "<p>html</p>", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"To: a@example.com, b@example.com\r\n", "Subject: =?utf-8?q?GitHub_digest_=E2=80=94_x?=\r\n", "multipart/alternative", "text/plain", "text/html"} {
		if !strings.Contains(s, want) {
			t.Errorf("message lacks %q", want)
		}
	}
	if strings.Index(s, "text/plain") > strings.Index(s, "text/html") {
		t.Error("the plain part comes first")
	}
}

func TestSummarySchemaIsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(summarySchema, &v); err != nil {
		t.Fatal(err)
	}
}
