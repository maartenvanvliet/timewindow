# timewindow

A small Go CLI that answers one question: **is this point in time allowed by a
policy of time-based rules?** It reports the answer on stdout and in its exit
status, so a shell, a Makefile, a cron job or a CI step can gate on it.

```console
$ timewindow
allowed=false
reason=friday-afternoon
$ echo $?
1
```

Policy is a list of **rules**, evaluated like firewall rules: top to bottom,
**last match wins**. That lets you start from a broad allow and layer narrower
denies (and then exceptions to those denies) after it, instead of bending every
policy into one fixed window shape.

Each rule matches on [Prometheus Alertmanager's](https://prometheus.io/docs/alerting/latest/configuration/#time_interval)
`time_intervals` schema — weekdays, months, days_of_month, years, times and
location are parsed by `github.com/prometheus/alertmanager/timeinterval`
itself, and matching is done by `timeinterval.NewIntervener(...).Mutes(...)`.
None of that logic is reimplemented here.

## Policy

`timewindow.yml`, the default `-config` path:

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

## Usage

```
timewindow [-config path] [-at time] [-format key-value|json] [-quiet] [-version]
```

| Flag | Meaning |
| --- | --- |
| `-config` | Policy file. Default `timewindow.yml`; `-` reads stdin. |
| `-at` | Evaluate an RFC 3339 instant instead of now — handy for asking "what will this policy say on Christmas Eve?". |
| `-format` | `key-value` (default) or `json`. |
| `-quiet` | Print nothing; answer with the exit status alone. |

The decision is reported twice, so callers can take whichever is convenient.
On stdout:

```console
$ timewindow -at 2026-12-24T10:00:00+01:00
allowed=false
reason=christmas-freeze

$ timewindow -at 2026-12-24T10:00:00+01:00 -format json
{"allowed":false,"reason":"christmas-freeze"}
```

...and in the exit status:

| Code | Meaning |
| --- | --- |
| `0` | Allowed. |
| `1` | Denied. |
| `2` | The policy or the arguments could not be used. The message goes to stderr. |

Which makes it a plain shell predicate:

```sh
timewindow -quiet && ./release.sh
```

The `reason` is the name of the rule that decided, or `no rule matched` when
the policy fell through to its default.

A broken policy never degrades to "allowed": it exits `2` with a message on
stderr, distinct from a real denial. Policies are rejected — rather than
quietly doing nothing — when a rule has no action or an unknown one, when a
rule defines no intervals (it could never match), when two rules share a name,
and when `action` is written one level too deep, under a `time_intervals` entry
instead of on the rule.

There is no override flag or env var: the tool only answers the question.
A break-glass path belongs to the caller, which can simply not call it.

## Install

Each release publishes static binaries for linux, macOS and Windows
(amd64/arm64), plus a `checksums.txt`. Grab one from the
[releases page](https://github.com/maartenvanvliet/timewindow/releases), or:

```sh
VERSION=1.0.0   # the tag is v1.0.0; archive names drop the v
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

curl -sSfL "https://github.com/maartenvanvliet/timewindow/releases/download/v${VERSION}/timewindow_${VERSION}_${OS}_${ARCH}.tar.gz" \
  | tar xz timewindow
```

The archives are flat, so `tar xz timewindow` pulls out just the binary. To
check it against the published checksums first:

```sh
curl -sSfLO "https://github.com/maartenvanvliet/timewindow/releases/download/v${VERSION}/timewindow_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -sSfL "https://github.com/maartenvanvliet/timewindow/releases/download/v${VERSION}/checksums.txt" \
  | grep "timewindow_${VERSION}_${OS}_${ARCH}.tar.gz" | sha256sum -c -
```

From source:

```sh
go install github.com/maartenvanvliet/timewindow@latest
```

Every binary reports its own provenance:

```console
$ timewindow -version
timewindow 1.0.0
commit: 0123456789ab
built:  2026-08-02T10:00:00Z
go:     go1.22.0 linux/amd64
```

Release builds get that from `-ldflags`; `go build` and `go install` binaries
fall back to the VCS data the Go toolchain stamps in, and mark a build from a
dirty tree as such.

## Integrations

Nothing about the tool is specific to CI — it is a predicate with an exit
status, so it composes wherever that works.

**A shell script:**

```sh
timewindow -quiet || { echo "outside the window, skipping"; exit 0; }
./release.sh
```

**A Makefile:**

```make
deploy:
	@timewindow -quiet || { echo "outside the deploy window"; exit 1; }
	./deploy.sh
```

**cron**, to run a job hourly but only while the policy allows it:

```cron
0 * * * * timewindow -quiet -config /etc/timewindow.yml && /usr/local/bin/run-batch
```

**A one-off question**, without touching a file at all:

```console
$ timewindow -config - -at 2026-12-24T10:00:00+01:00 <<'EOF'
rules:
  - name: christmas-freeze
    action: deny
    time_intervals: [{ months: ['december'], days_of_month: ['22:31'] }]
EOF
allowed=false
reason=christmas-freeze
```

### GitHub Actions

The `key=value` output is the format `$GITHUB_OUTPUT` wants, so a step can
append to it directly:

```yaml
- name: Install timewindow
  run: |
    curl -sSfL https://github.com/maartenvanvliet/timewindow/releases/download/v1.0.0/timewindow_1.0.0_linux_amd64.tar.gz \
      | tar xz timewindow
- id: check
  run: |
    set +e
    ./timewindow >> "$GITHUB_OUTPUT"
    code=$?
    # 0 = allowed, 1 = denied: both are valid answers, keep going.
    # 2 = broken policy: fail the workflow.
    [ "$code" -le 1 ] || exit "$code"
```

Then gate the job on the step output:

```yaml
- name: Deploy to ECS
  if: steps.check.outputs.allowed == 'true'
  run: ./deploy.sh
```

Pin the version in the URL rather than tracking `latest`, so a new release
cannot change a decision without a commit to your workflow.

To let a human force a run, take a `workflow_dispatch` input and skip the
check — the override belongs to the caller, where it is recorded against the
workflow run:

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
    ./timewindow >> "$GITHUB_OUTPUT"
    code=$?
    [ "$code" -le 1 ] || exit "$code"
```

In a repo that vendors this tool, `go run . >> "$GITHUB_OUTPUT"` works too, but
it cannot tell "correctly skipped" from "broken policy" — `go run` reports
every non-zero program exit as `1`. Use the released binary, or `go build`
first, when you need the exit codes intact.

## Releasing

Run the **release** workflow from the Actions tab and pick `patch`, `minor` or
`major`. It works out the next version from the highest existing tag, tags the
commit and publishes the release — no local tagging step:

```
v1.2.3  --patch-->  v1.2.4
        --minor-->  v1.3.0
        --major-->  v2.0.0
```

The `version` input overrides that when you want an exact tag, including a
pre-release like `v1.0.0-rc.1`, which is published as one. Tagging by hand
still works and takes the same path:

```sh
git tag -a v1.0.0 -m v1.0.0 && git push origin v1.0.0
```

Either way the workflow checks formatting, runs `go vet` and the tests, and
only then hands off to [GoReleaser](https://goreleaser.com), which builds every
target, packages the archives and checksums, and publishes them with
GitHub-generated notes. Nothing is tagged or published from a red tree.

To see what a release would contain, without tagging or publishing anything:

```sh
goreleaser check                       # validate .goreleaser.yml
goreleaser release --snapshot --clean  # writes dist/
```

Builds are `CGO_ENABLED=0 -trimpath`, so the binaries are static and carry no
local paths.

## Tests

```
go test ./...
```

Table-driven cases cover office hours, the Friday cutoff, the weekend, both
halves of the Christmas freeze, the DST switch, and the engine itself: a later
deny beating an earlier allow, the same two rules swapped producing the
opposite outcome, an allow punching a hole in an earlier deny, the
always-matching empty interval, and both defaults. Policy errors are covered by
their own table — malformed YAML, the pre-rules schema, a bad timezone, bad or
missing actions, a rule with no intervals, duplicate names and a misplaced
nested `action`.

`run` is the entire CLI, so the argument handling is tested the way a caller
sees it: arguments in, stdout/stderr and an exit code out. That table covers
both output formats, `-quiet`, a bad `-at` and `-format`, an unknown flag, and
that `-h` is not an error. A third table covers the version stamping: link time
values winning over the toolchain's, a `go install` build describing itself
from the VCS stamps, and a dirty tree being flagged.

## Notes

- The pre-rules schema (top-level `time_intervals` with
  `active_time_intervals` / `mute_time_intervals`) is rejected with a
  migration error rather than ignored, so an old policy cannot silently
  evaluate to an empty rule chain.
- Parsing is non-strict, so unknown keys elsewhere in the document are
  tolerated. Note that YAML key matching is case-sensitive here: `Action:` is
  not the same key as `action:`. Both spellings are caught inside a
  `time_intervals` entry, since silently dropping one would change a decision.
- `github.com/prometheus/alertmanager` is pinned to `v0.28.1` — the newest
  release whose module still builds on Go 1.22 while exposing
  `Mutes(names []string, now time.Time) (bool, []string, error)`. Later
  releases (`v0.29.0`+) require Go 1.24 or newer; bump `go-version` in the
  release workflow if you upgrade.
