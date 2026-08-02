# deploy-gate

A small Go CLI that decides whether a production deployment is allowed to run
right now. It is meant to be a step in a GitHub Actions workflow, in front of a
deploy job.

Policy is a list of **rules**, evaluated like firewall rules: top to bottom,
**last match wins**. That lets you start from a broad allow and layer narrower
denies (and then exceptions to those denies) after it, instead of bending every
policy into one fixed window shape.

Each rule matches on [Prometheus Alertmanager's](https://prometheus.io/docs/alerting/latest/configuration/#time_interval)
`time_intervals` schema — weekdays, months, days_of_month, years, times and
location are parsed by `github.com/prometheus/alertmanager/timeinterval`
itself, and matching is done by `timeinterval.NewIntervener(...).Mutes(...)`.
None of that logic is reimplemented here.

## Config

`.github/deploy-window.yml`:

```yaml
timezone: Europe/Amsterdam

# Action when no rule below matches.
default: deny

rules:
  # Deploying is fine during office hours on a weekday...
  - name: office-hours
    action: allow
    time_intervals:
      - weekdays: ['monday:friday']
        times: [{ start_time: '09:00', end_time: '18:00' }]

  # ...except that Friday afternoon is too late to babysit a rollback.
  - name: friday-afternoon
    action: deny
    time_intervals:
      - weekdays: ['friday']
        times: [{ start_time: '14:00', end_time: '24:00' }]

  # Nothing ships over the Christmas break.
  - name: christmas-freeze
    action: deny
    time_intervals:
      - months: ['december']
        days_of_month: ['22:31']
      - months: ['january']
        days_of_month: ['1:2']
```

| Key | Meaning |
| --- | --- |
| `timezone` | Default location for intervals that do not set `location` themselves. Omitted ⇒ intervals are matched in UTC. |
| `default` | `allow` or `deny`, applied when no rule matches. Optional; **`deny`** when omitted, so a partial or empty policy fails closed. |
| `rules[].name` | Reported as the `reason`. Optional; an unnamed rule is called `rule[0]` after its position. Names must be unique. |
| `rules[].action` | `allow` or `deny`. Required. |
| `rules[].time_intervals` | Alertmanager intervals, OR'ed together: the rule matches when the current time falls in any of them. |

### How the chain evaluates

Every rule is tested against the current time. Matches do not short-circuit —
the **last** matching rule decides, and its name becomes the reason. So in the
config above, Friday 15:00 matches both `office-hours` and `friday-afternoon`,
and the later one wins:

```
allowed=false
reason=friday-afternoon
```

Order is the whole policy. The same two rules swapped give the opposite answer,
which is the point: put the broad strokes first and the exceptions last.

`time_intervals: [{}]` — one empty interval — matches every time, which is how
you write an unconditional base rule and layer exceptions on top:

```yaml
default: deny
rules:
  - name: always                # broad allow
    action: allow
    time_intervals: [{}]
  - name: christmas-freeze      # narrower deny
    action: deny
    time_intervals:
      - months: ['december']
        days_of_month: ['22:31']
  - name: freeze-hotfix-window  # exception to the deny
    action: allow
    time_intervals:
      - months: ['december']
        days_of_month: ['27']
        times: [{ start_time: '10:00', end_time: '12:00' }]
```

The IANA database is embedded (`time/tzdata`), so `Europe/Amsterdam` resolves
even on a minimal CI image without `/usr/share/zoneinfo`, and intervals are
matched against local wall-clock time across DST switches.

## Behaviour

```
deploy-gate [-config .github/deploy-window.yml]
```

Two `KEY=value` lines on stdout, ready to be redirected into `$GITHUB_OUTPUT`:

```
allowed=true|false
reason=<name of the deciding rule | "no rule matched">
```

Exit codes:

| Code | Meaning |
| --- | --- |
| `0` | Allowed. |
| `1` | Denied. |
| `2` | Config missing, unparseable or invalid. The error goes to stderr. |

A config error never degrades to "allowed": it exits `2` with a message on
stderr, so a broken config fails the workflow instead of waving a deploy
through. Configs are rejected — rather than quietly doing nothing — when a rule
has no action or an unknown one, when a rule defines no intervals (it could
never match), when two rules share a name, and when `action` is written one
level too deep, under a `time_intervals` entry instead of on the rule.

There is no override flag or env var: the tool only answers the question. If
you want a break-glass path, skip the call in the shell — see below.

## Install

Each release publishes static binaries for linux, macOS and Windows
(amd64/arm64), plus a `checksums.txt`. Grab one from the
[releases page](https://github.com/maartenvanvliet/timewindow/releases), or:

```sh
VERSION=v1.0.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

curl -sSfL "https://github.com/maartenvanvliet/timewindow/releases/download/${VERSION}/deploy-gate_${VERSION}_${OS}_${ARCH}.tar.gz" \
  | tar xz deploy-gate
```

The archives are flat, so `tar xz deploy-gate` pulls out just the binary. To
check it against the published checksums first:

```sh
curl -sSfLO "https://github.com/maartenvanvliet/timewindow/releases/download/${VERSION}/deploy-gate_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -sSfL "https://github.com/maartenvanvliet/timewindow/releases/download/${VERSION}/checksums.txt" \
  | grep "deploy-gate_${VERSION}_${OS}_${ARCH}.tar.gz" | sha256sum -c -
```

From source, either of:

```sh
go build -o deploy-gate .                                  # in a checkout
go install github.com/maartenvanvliet/timewindow@latest    # installs as `timewindow`
```

`go install` names the binary after the module, so it lands as `timewindow`
rather than `deploy-gate`. Every binary reports its own provenance:

```console
$ deploy-gate -version
deploy-gate v1.0.0
commit: 0123456789ab
built:  2026-08-02T10:00:00Z
go:     go1.22.0 linux/amd64
```

Release builds get that from `-ldflags`; `go build` and `go install` binaries
fall back to the VCS data the Go toolchain stamps in, and mark a build from a
dirty tree as such.

## GitHub Actions

```yaml
- name: Install deploy-gate
  run: |
    curl -sSfL https://github.com/maartenvanvliet/timewindow/releases/download/v1.0.0/deploy-gate_v1.0.0_linux_amd64.tar.gz \
      | tar xz deploy-gate
- id: check
  run: |
    set +e
    ./deploy-gate >> "$GITHUB_OUTPUT"
    code=$?
    # 0 = allowed, 1 = denied: both are valid answers, keep going.
    # 2 = broken config: fail the workflow.
    [ "$code" -le 1 ] || exit "$code"
```

Then gate the deploy job on the step output:

```yaml
- name: Deploy to ECS
  if: steps.check.outputs.allowed == 'true'
  run: ./deploy.sh
```

Pin the version in the URL rather than tracking `latest`, so a new release
cannot change a deploy decision without a commit to your workflow.

### Break-glass

Overrides belong to the caller, not the gate. To let a human force a deploy,
take a `workflow_dispatch` input and skip the check:

```yaml
on:
  workflow_dispatch:
    inputs:
      force:
        description: Deploy even outside the window
        type: boolean
        default: false
```

```yaml
- id: check
  env:
    FORCE: ${{ inputs.force }}
  run: |
    if [ "$FORCE" = "true" ]; then
      printf 'allowed=true\nreason=manual override\n' >> "$GITHUB_OUTPUT"
      exit 0
    fi
    set +e
    ./deploy-gate >> "$GITHUB_OUTPUT"
    code=$?
    [ "$code" -le 1 ] || exit "$code"
```

That keeps the override where it can be seen and audited — in the workflow
run's inputs — instead of in an environment variable that anything on the
runner could have set.

In a repo that already vendors this tool, you can skip the download:

```yaml
- uses: actions/setup-go@v5
  with: { go-version: '1.22' }
- id: check
  run: go run . >> "$GITHUB_OUTPUT"
```

That form cannot tell "correctly skipped" from "broken config", though —
`go run` reports every non-zero program exit as `1`. Use the released binary,
or `go build` first, when you need the exit codes intact.

## Releasing

Tag and push; [`.github/workflows/release.yml`](.github/workflows/release.yml)
does the rest:

```sh
git tag -a v1.0.0 -m 'v1.0.0'
git push origin v1.0.0
```

The workflow checks formatting, runs `go vet` and the tests, builds the
archives and publishes them with generated release notes. A tag containing a
hyphen (`v1.0.0-rc.1`) is published as a pre-release. Nothing is published if
the tests fail.

The build itself is a plain script, so a release can be reproduced locally:

```sh
VERSION=v1.0.0 ./script/build-release.sh   # writes dist/
```

Builds are `CGO_ENABLED=0 -trimpath`, so the binaries are static and free of
local paths; `SOURCE_DATE_EPOCH` is honoured if you want the timestamp fixed
too.

## Tests

```
go test ./...
```

Table-driven cases cover office hours, the Friday cutoff, the weekend, both
halves of the Christmas freeze, the DST switch, and the engine itself: a later deny beating an earlier allow, the same two rules
swapped producing the opposite outcome, an allow punching a hole in an earlier
deny, the always-matching empty interval, and both defaults. Config errors are
covered by their own table — malformed YAML, the pre-rules schema, a bad
timezone, bad or missing actions, a rule with no intervals, duplicate names and
a misplaced nested `action`. A third table covers the version stamping: link
time values winning over the toolchain's, a `go install` build describing
itself from the VCS stamps, and a dirty tree being flagged.

## Notes

- The pre-rules schema (top-level `time_intervals` with
  `active_time_intervals` / `mute_time_intervals`) is rejected with a
  migration error rather than ignored, so an old config cannot silently
  evaluate to an empty rule chain.
- Parsing is non-strict, so unknown keys elsewhere in the document are
  tolerated. Note that YAML key matching is case-sensitive here: `Action:` is
  not the same key as `action:`. Both spellings are caught inside a
  `time_intervals` entry, since silently dropping one would change a decision.
- `github.com/prometheus/alertmanager` is pinned to `v0.28.1` — the newest
  release whose module still builds on Go 1.22 while exposing
  `Mutes(names []string, now time.Time) (bool, []string, error)`. Later
  releases (`v0.29.0`+) require Go 1.24 or newer; bump `go-version` in the
  workflow above if you upgrade.
