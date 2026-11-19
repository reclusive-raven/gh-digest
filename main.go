// Command gh-digest emails a day's GitHub activity, one digest per source.
// It also runs as the gh extension `gh digest`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

const usage = `usage: gh digest [flags] <source>...

A source is an organisation, a user, or one owner/repo. Prints one digest per
source for a calendar day; with --to, also emails it over SMTP.

Flags:
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) && !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, "gh-digest:", err)
		}
		os.Exit(1)
	}
}

// listFlag collects comma-separated or repeated values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

func cli(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gh-digest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
		fmt.Fprint(stderr, `
Environment:
  GH_TOKEN, GITHUB_TOKEN  GitHub token (default: gh auth token); needs read access to every source
  ANTHROPIC_API_KEY       turns on the Claude summary; without it the digest lists events
  SMTP_USER, SMTP_PASSWORD  the sender and its password, needed with --to
  SMTP_HOST, SMTP_PORT    default smtp.gmail.com and 465 (implicit TLS)
  TZ                      the zone whose calendar day is reported, when --tz is not given
`)
	}
	var to, alsoTo listFlag
	date := fs.String("date", "", "the day to report, YYYY-MM-DD (default: yesterday)")
	tz := fs.String("tz", "", "IANA time zone of the day (default: the local zone)")
	fs.Var(&to, "to", "email the digests to these addresses (comma-separated or repeated)")
	fs.Var(&alsoTo, "also-to", "source=address: an extra recipient for one source's digest (repeatable)")
	sendOnEmpty := fs.Bool("send-on-empty", false, "send a digest for a source with no activity")
	html := fs.Bool("html", false, "print the HTML instead of the plain text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	o := options{Sources: fs.Args(), To: to, AlsoTo: map[string][]string{}, SendOnEmpty: *sendOnEmpty, HTML: *html, Attempts: 3}
	if len(o.Sources) == 0 {
		fs.Usage()
		return errUsage
	}

	o.Loc, o.TZ = time.Local, getenv("TZ")
	if *tz != "" {
		loc, err := time.LoadLocation(*tz)
		if err != nil {
			return fmt.Errorf("--tz: %w", err)
		}
		o.Loc, o.TZ = loc, *tz
	}
	if o.TZ == "" {
		o.TZ = o.Loc.String()
	}
	if *date == "" {
		now := time.Now().In(o.Loc)
		o.Date = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, o.Loc).AddDate(0, 0, -1)
	} else {
		d, err := time.ParseInLocation("2006-01-02", *date, o.Loc)
		if err != nil {
			return fmt.Errorf("--date: want YYYY-MM-DD: %w", err)
		}
		o.Date = d
	}

	for _, a := range to {
		if err := plainAddress(a); err != nil {
			return fmt.Errorf("--to: %w", err)
		}
	}
	for _, pair := range alsoTo {
		source, addr, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(source) == "" {
			return fmt.Errorf("--also-to: want source=address, got %q", pair)
		}
		if err := plainAddress(addr); err != nil {
			return fmt.Errorf("--also-to: %w", err)
		}
		if !hasSource(o.Sources, source) {
			return fmt.Errorf("--also-to: %s is not one of the sources", source)
		}
		key := strings.ToLower(strings.TrimSpace(source))
		o.AlsoTo[key] = append(o.AlsoTo[key], strings.TrimSpace(addr))
	}
	if len(to) == 0 && len(alsoTo) > 0 {
		return errors.New("--also-to needs --to")
	}

	var send sender
	if len(to) > 0 {
		user, pass := getenv("SMTP_USER"), strings.Join(strings.Fields(getenv("SMTP_PASSWORD")), "")
		if user == "" || pass == "" {
			return errors.New("--to needs SMTP_USER and SMTP_PASSWORD")
		}
		host, port := getenv("SMTP_HOST"), getenv("SMTP_PORT")
		if host == "" {
			host = "smtp.gmail.com"
		}
		if port == "" {
			port = "465"
		}
		send = smtpSender(host, port, user, pass)
	}

	token, err := githubToken(getenv)
	if err != nil {
		return err
	}
	var sum summarizer
	if key := getenv("ANTHROPIC_API_KEY"); key != "" {
		prompt, err := files.ReadFile("prompt.md")
		if err != nil {
			return err
		}
		sum = claudeSummarizer(anthropic.NewClient(option.WithAPIKey(key)), string(prompt), 5*time.Minute)
	}
	return run(ctx, newClient(token), sum, send, o, stdout, stderr)
}

// plainAddress accepts a bare address only, never a display name or header.
func plainAddress(a string) error {
	p, err := mail.ParseAddress(a)
	if err != nil || p.Name != "" || p.Address != strings.TrimSpace(a) {
		return fmt.Errorf("%q is not a plain email address", a)
	}
	return nil
}

func hasSource(sources []string, s string) bool {
	for _, x := range sources {
		if strings.EqualFold(x, strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

// githubToken is GH_TOKEN or GITHUB_TOKEN, else gh's own login.
func githubToken(getenv func(string) string) (string, error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if t := strings.TrimSpace(getenv(k)); t != "" {
			return t, nil
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if t := strings.TrimSpace(string(out)); err == nil && t != "" {
		return t, nil
	}
	return "", errors.New("no GitHub token: set GH_TOKEN or sign in with gh auth login")
}
