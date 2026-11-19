# gh-digest

A daily email of what happened on GitHub: one digest per source (an organisation, a user, or a single repository) for one calendar day. Each digest says what shipped and what is in progress per repository, optionally summarised by Claude, with every line linked to the commit, pull request or issue behind it.

It runs once and exits, with no database, so it fits a cron job or a scheduled GitHub Actions workflow. It is also a [gh](https://cli.github.com) extension.

## Install

```sh
gh extension install reclusive-raven/gh-digest
```

or build it with Go:

```sh
go install github.com/reclusive-raven/gh-digest@latest
```

## Use

```sh
gh digest my-org                               # yesterday's digest for my-org, printed
gh digest --date 2026-09-24 my-org some-user owner/repo
gh digest --html my-org > digest.html
gh digest --to me@example.com --also-to my-org=teammate@example.com my-org other-org
```

| Flag | Meaning |
| --- | --- |
| `--date YYYY-MM-DD` | The day to report. Default: yesterday. |
| `--tz Zone/Name` | The time zone whose calendar day is reported. Default: the local zone (`TZ`). |
| `--to a,b` | Email each digest to these addresses. Without it, digests are only printed. |
| `--also-to source=addr` | An extra recipient for one source's digest. Repeatable. |
| `--send-on-empty` | Send a digest for a source with no activity that day. |
| `--html` | Print the HTML instead of the plain text. |

| Environment | Meaning |
| --- | --- |
| `GH_TOKEN` or `GITHUB_TOKEN` | A token that can read every source. Default: `gh auth token`. A classic token with `repo` and `read:org`, or a fine-grained token with read access to contents, pull requests and issues. |
| `ANTHROPIC_API_KEY` | Turns on the summary. Without it, the digest lists the day's events per repository. |
| `SMTP_USER`, `SMTP_PASSWORD` | Needed with `--to`. The user is also the sender. For Gmail, use an [App Password](https://myaccount.google.com/apppasswords). |
| `SMTP_HOST`, `SMTP_PORT` | Default `smtp.gmail.com` and `465` (implicit TLS). |

## What it reads

For each repository of a source (archived ones are skipped), inside the calendar day: commits on the default branch, pull requests opened, merged or closed without merging, and issues opened or closed. A user source covers the user's own repositories, private ones included when the token can see them.

## Behaviour worth knowing

- **All or nothing.** Every source is read, and summarised, before any email is sent. If any repository cannot be read completely, or a summary still fails its check after three attempts, the run exits non-zero and sends nothing, so rerunning the same `--date` never repeats an email. If sending itself fails part way, the error names the sources already sent; rerun with the rest.
- **Quiet days send nothing** unless `--send-on-empty` is set.
- **Summaries are checked.** Claude (`claude-sonnet-5-5`, through the Messages API with structured output) must cite a collected event for every line it writes, and may cite only events of the repository it is describing. Links are built from the collected events, never taken from the model. A summary that fails the check is rejected.
- **Output.** stdout gets the digests; stderr gets one status line per source and never digest text, so stdout can be discarded in CI logs.
- **Rate limits** fail the run with the time to retry after; nothing waits or retries on its own.

## Run it daily with GitHub Actions

Keep the token, the SMTP password and the API key in Actions secrets, and discard stdout so digest content stays out of the logs.

```yaml
name: digest
on:
  schedule:
    - cron: "45 22 * * *"   # 04:15 the next morning in UTC+5:30
  workflow_dispatch:
    inputs:
      date:
        description: "YYYY-MM-DD (default: yesterday)"
        required: false
permissions:
  contents: read
jobs:
  digest:
    runs-on: ubuntu-latest
    env:
      TZ: Asia/Kolkata        # the zone whose calendar day is reported
      GH_TOKEN: ${{ secrets.DIGEST_GITHUB_TOKEN }}
      ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
      SMTP_USER: ${{ secrets.SMTP_USER }}
      SMTP_PASSWORD: ${{ secrets.SMTP_PASSWORD }}
      DATE: ${{ inputs.date }}
    steps:
      - run: gh extension install reclusive-raven/gh-digest --pin v0.1.1
      - run: gh digest ${DATE:+--date "$DATE"} --to me@example.com my-org > /dev/null
```

## Licence

MIT
