# deploy-gate

A small Go CLI that decides whether a production deployment is allowed to run
right now. It is meant to be a step in a GitHub Actions workflow, in front of a
deploy job.

The config reuses [Prometheus Alertmanager's](https://prometheus.io/docs/alerting/latest/configuration/#time_interval)
`time_intervals` schema verbatim — weekdays, months, days_of_month, years,
times and location are parsed by `github.com/prometheus/alertmanager/timeinterval`
itself, and all matching is done by `timeinterval.NewIntervener(...).Mutes(...)`.
None of that logic is reimplemented here.

## Config

`.github/deploy-window.yml`:

```yaml
timezone: Europe/Amsterdam

active_time_intervals:
  - office-hours
mute_time_intervals:
  - christmas-freeze

time_intervals:
  - name: office-hours
    time_intervals:
      - weekdays: ['monday:thursday']
        times: [{ start_time: '09:00', end_time: '18:00' }]
      - weekdays: ['friday']
        times: [{ start_time: '09:00', end_time: '14:00' }]
  - name: christmas-freeze
    time_intervals:
      - months: ['december']
        days_of_month: ['22:31']
        Action: deny
      - months: ['january']
        days_of_month: ['1:2']
        Action: deny
```

| Key | Meaning |
| --- | --- |
| `timezone` | Default location for intervals that do not set `location` themselves. |
| `time_intervals` | Named interval definitions, in Alertmanager's schema. |
| `active_time_intervals` | Windows in which deploying is allowed. Empty ⇒ no window restriction. |
| `mute_time_intervals` | Windows in which deploying is blocked. |

Deploying is allowed when **(no `active_time_intervals` are configured, or the
current time matches at least one)** and **the current time matches no
`mute_time_intervals`**. A mute match always wins.

The IANA database is embedded (`time/tzdata`), so `Europe/Amsterdam` resolves
even on a minimal CI image without `/usr/share/zoneinfo`.

## Behaviour

```
deploy-gate [-config .github/deploy-window.yml]
```

Two `KEY=value` lines on stdout, ready to be redirected into `$GITHUB_OUTPUT`:

```
allowed=true|false
reason=<matched interval name | "manual override" | "no active window matched" | "no deploy window configured">
```

Exit codes:

| Code | Meaning |
| --- | --- |
| `0` | Allowed. |
| `1` | Denied — outside every active window, or inside a mute window. |
| `2` | Config missing, unparseable, referencing an undefined interval name, or carrying an invalid timezone. The error goes to stderr. |

A config error never degrades to "allowed": it exits `2` with a message on
stderr, so a broken config fails the workflow instead of waving a deploy
through.

Set `DEPLOY_GATE_OVERRIDE` to force a deploy through a closed window. Any value
other than empty, `0`, `false` or `no` (case-insensitive) counts as set; the
config is not even read in that case, and the tool prints
`allowed=true` / `reason=manual override`.

## GitHub Actions

```yaml
- uses: actions/setup-go@v5
  with: { go-version: '1.22' }
- id: check
  run: go run . >> "$GITHUB_OUTPUT"
```

Then gate the deploy job on the step output:

```yaml
- name: Deploy to ECS
  if: steps.check.outputs.allowed == 'true'
  run: ./deploy.sh
```

Note that `go run .` reports every non-zero program exit as `1`, so the step
above cannot tell "correctly skipped" from "broken config". Build the binary
first when you want that distinction:

```yaml
- uses: actions/setup-go@v5
  with: { go-version: '1.22' }
- id: check
  run: |
    go build -o deploy-gate .
    set +e
    ./deploy-gate >> "$GITHUB_OUTPUT"
    code=$?
    # 0 = allowed, 1 = denied: both are valid answers, keep going.
    # 2 = broken config: fail the workflow.
    [ "$code" -le 1 ] || exit "$code"
```

## Tests

```
go test ./...
```

Table-driven cases cover office hours, Friday after the 14:00 close, the
weekend, both halves of the Christmas freeze, the override env var, an unknown
interval name in the policy, malformed YAML and an invalid timezone.

## Notes

- `Action: deny` in the example config is **not** part of Alertmanager's
  schema. Parsing is non-strict so keys like it are ignored rather than
  rejected; deny semantics come from listing the interval under
  `mute_time_intervals`.
- `github.com/prometheus/alertmanager` is pinned to `v0.28.1` — the newest
  release whose module still builds on Go 1.22 while exposing
  `Mutes(names []string, now time.Time) (bool, []string, error)`. Later
  releases (`v0.29.0`+) require Go 1.24 or newer; bump `go-version` in the
  workflow above if you upgrade.
