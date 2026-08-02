package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// officeConfig mirrors the shipped policy: a broad weekday allow, with a
// Friday cutoff and a Christmas freeze layered after it.
const officeConfig = `
timezone: Europe/Amsterdam
default: deny

rules:
  - name: office-hours
    action: allow
    time_intervals:
      - weekdays: ['monday:friday']
        times: [{ start_time: '09:00', end_time: '18:00' }]
  - name: friday-afternoon
    action: deny
    time_intervals:
      - weekdays: ['friday']
        times: [{ start_time: '14:00', end_time: '24:00' }]
  - name: christmas-freeze
    action: deny
    time_intervals:
      - months: ['december']
        days_of_month: ['22:31']
      - months: ['january']
        days_of_month: ['1:2']
`

// reAllowConfig layers an allow after a deny, to show that a later rule can
// punch a hole in an earlier one.
const reAllowConfig = `
timezone: Europe/Amsterdam
default: deny

rules:
  - name: always
    action: allow
    time_intervals:
      - {}
  - name: christmas-freeze
    action: deny
    time_intervals:
      - months: ['december']
        days_of_month: ['22:31']
  - name: freeze-hotfix-window
    action: allow
    time_intervals:
      - months: ['december']
        days_of_month: ['27']
        times: [{ start_time: '10:00', end_time: '12:00' }]
`

// allowThenDeny and denyThenAllow hold the same two rules in either order.
const allowThenDeny = `
timezone: Europe/Amsterdam
rules:
  - name: weekdays
    action: allow
    time_intervals:
      - weekdays: ['monday:friday']
  - name: fridays
    action: deny
    time_intervals:
      - weekdays: ['friday']
`

const denyThenAllow = `
timezone: Europe/Amsterdam
rules:
  - name: fridays
    action: deny
    time_intervals:
      - weekdays: ['friday']
  - name: weekdays
    action: allow
    time_intervals:
      - weekdays: ['monday:friday']
`

const malformedConfig = `
timezone: Europe/Amsterdam
rules:
  - name: office-hours
   action: allow
`

const legacyConfig = `
timezone: Europe/Amsterdam
active_time_intervals: ['office-hours']
time_intervals:
  - name: office-hours
    time_intervals:
      - weekdays: ['monday:friday']
`

// amsterdam returns a local wall-clock time in Europe/Amsterdam.
func amsterdam(t *testing.T, value string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("loading Europe/Amsterdam: %v", err)
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return ts
}

// writeConfig writes contents to a temp file and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy-window.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name       string
		config     string
		now        string // wall clock in Europe/Amsterdam
		override   string
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "within office hours on a wednesday",
			config:     officeConfig,
			now:        "2026-03-11 10:30",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "friday before the cutoff",
			config:     officeConfig,
			now:        "2026-03-13 13:59",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "friday after 14:00 hits the layered deny",
			config:     officeConfig,
			now:        "2026-03-13 14:01",
			wantAllow:  false,
			wantReason: "friday-afternoon",
		},
		{
			name:       "weekend falls through to the default",
			config:     officeConfig,
			now:        "2026-03-14 11:00",
			wantAllow:  false,
			wantReason: reasonNoMatch,
		},
		{
			name:       "before opening time falls through to the default",
			config:     officeConfig,
			now:        "2026-03-11 08:59",
			wantAllow:  false,
			wantReason: reasonNoMatch,
		},
		{
			name:       "christmas freeze overrides office hours",
			config:     officeConfig,
			now:        "2026-12-23 10:00",
			wantAllow:  false,
			wantReason: "christmas-freeze",
		},
		{
			name:       "january tail of the freeze overrides office hours",
			config:     officeConfig,
			now:        "2027-01-01 10:00",
			wantAllow:  false,
			wantReason: "christmas-freeze",
		},
		{
			name:       "just after the freeze ends",
			config:     officeConfig,
			now:        "2027-01-04 10:00",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "monday after the DST switch still opens at 09:00 local",
			config:     officeConfig,
			now:        "2026-03-30 09:30",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "override wins over a denying rule",
			config:     officeConfig,
			now:        "2026-12-23 10:00",
			override:   "true",
			wantAllow:  true,
			wantReason: "manual override",
		},
		{
			name:       "falsey override does not force a deploy",
			config:     officeConfig,
			now:        "2026-12-23 10:00",
			override:   "false",
			wantAllow:  false,
			wantReason: "christmas-freeze",
		},

		// Last match wins.
		{
			name:       "later deny beats an earlier allow",
			config:     allowThenDeny,
			now:        "2026-03-13 10:00",
			wantAllow:  false,
			wantReason: "fridays",
		},
		{
			name:       "same rules in the other order flip the outcome",
			config:     denyThenAllow,
			now:        "2026-03-13 10:00",
			wantAllow:  true,
			wantReason: "weekdays",
		},
		{
			name:       "an allow can punch a hole in an earlier deny",
			config:     reAllowConfig,
			now:        "2026-12-27 10:30",
			wantAllow:  true,
			wantReason: "freeze-hotfix-window",
		},
		{
			name:       "outside the hole the earlier deny still stands",
			config:     reAllowConfig,
			now:        "2026-12-27 13:00",
			wantAllow:  false,
			wantReason: "christmas-freeze",
		},
		{
			name:       "an empty interval matches every time",
			config:     reAllowConfig,
			now:        "2026-07-05 03:00",
			wantAllow:  true,
			wantReason: "always",
		},

		// Defaults.
		{
			name:       "default is deny when omitted",
			config:     "rules: []\n",
			now:        "2026-03-11 10:30",
			wantAllow:  false,
			wantReason: reasonNoMatch,
		},
		{
			name:       "default allow lets unmatched times through",
			config:     "default: allow\n" + strings.TrimPrefix(allowThenDeny, "\n"),
			now:        "2026-03-14 10:00",
			wantAllow:  true,
			wantReason: reasonNoMatch,
		},
		{
			name:       "an empty config denies rather than waving deploys through",
			config:     "timezone: Europe/Amsterdam\n",
			now:        "2026-03-11 10:30",
			wantAllow:  false,
			wantReason: reasonNoMatch,
		},

		// Naming.
		{
			name: "unnamed rules are reported by position",
			config: `
rules:
  - action: allow
    time_intervals:
      - {}
`,
			now:        "2026-03-11 10:30",
			wantAllow:  true,
			wantReason: "rule[0]",
		},

		// Without a timezone, intervals are matched in UTC.
		{
			name: "no timezone means UTC",
			config: `
rules:
  - name: utc-office-hours
    action: allow
    time_intervals:
      - times: [{ start_time: '09:00', end_time: '18:00' }]
`,
			now:        "2026-03-11 09:30", // 08:30 UTC
			wantAllow:  false,
			wantReason: reasonNoMatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.config)

			got, err := Evaluate(path, amsterdam(t, tc.now), tc.override)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.Allowed != tc.wantAllow || got.Reason != tc.wantReason {
				t.Errorf("Evaluate() = {allowed:%t reason:%q}, want {allowed:%t reason:%q}",
					got.Allowed, got.Reason, tc.wantAllow, tc.wantReason)
			}
		})
	}
}

func TestEvaluateConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string // substring of the error
	}{
		{
			name:    "malformed yaml",
			config:  malformedConfig,
			wantErr: "parsing config",
		},
		{
			name:    "pre-rules schema",
			config:  legacyConfig,
			wantErr: "no longer supported",
		},
		{
			name:    "invalid timezone",
			config:  "timezone: Mars/Olympus_Mons\n",
			wantErr: "invalid timezone",
		},
		{
			name:    "invalid default action",
			config:  "default: maybe\n",
			wantErr: `default: action must be "allow" or "deny", got "maybe"`,
		},
		{
			name: "missing rule action",
			config: `
rules:
  - name: office-hours
    time_intervals:
      - weekdays: ['monday']
`,
			wantErr: `rule "office-hours": action must be "allow" or "deny"`,
		},
		{
			name: "unknown rule action",
			config: `
rules:
  - name: office-hours
    action: mute
    time_intervals:
      - weekdays: ['monday']
`,
			wantErr: `rule "office-hours": action must be "allow" or "deny", got "mute"`,
		},
		{
			name: "rule without intervals can never match",
			config: `
rules:
  - name: office-hours
    action: allow
`,
			wantErr: `rule "office-hours": no time_intervals defined`,
		},
		{
			name: "duplicate rule names",
			config: `
rules:
  - name: office-hours
    action: allow
    time_intervals:
      - weekdays: ['monday']
  - name: office-hours
    action: deny
    time_intervals:
      - weekdays: ['friday']
`,
			wantErr: `duplicate rule name "office-hours"`,
		},
		{
			name: "action nested under time_intervals",
			config: `
rules:
  - name: christmas-freeze
    action: deny
    time_intervals:
      - months: ['december']
        action: deny
`,
			wantErr: "`action` belongs on the rule",
		},
		{
			name: "capitalised action nested under time_intervals",
			config: `
rules:
  - name: christmas-freeze
    action: deny
    time_intervals:
      - months: ['december']
        Action: deny
`,
			wantErr: "`action` belongs on the rule",
		},
		{
			name: "invalid interval body",
			config: `
rules:
  - name: office-hours
    action: allow
    time_intervals:
      - weekdays: ['funday']
`,
			wantErr: "parsing config",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.config)

			got, err := Evaluate(path, time.Now(), "")
			if err == nil {
				t.Fatalf("Evaluate() = %+v, want error", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestEvaluateMissingConfig covers the "broken config" path when the file does
// not exist at all: it must error rather than allow.
func TestEvaluateMissingConfig(t *testing.T) {
	_, err := Evaluate(filepath.Join(t.TempDir(), "absent.yml"), time.Now(), "")
	if err == nil {
		t.Fatal("Evaluate() with a missing config = nil error, want an error")
	}
}

// TestEvaluateOverrideSkipsConfig documents that an override does not require a
// readable config: the gate is bypassed entirely.
func TestEvaluateOverrideSkipsConfig(t *testing.T) {
	got, err := Evaluate(filepath.Join(t.TempDir(), "absent.yml"), time.Now(), "1")
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !got.Allowed || got.Reason != "manual override" {
		t.Errorf("Evaluate() = %+v, want an allowed manual override", got)
	}
}

// TestShippedConfig guards the config committed at .github/deploy-window.yml.
func TestShippedConfig(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(".github", "deploy-window.yml"))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	tests := []struct {
		now        string
		wantAllow  bool
		wantReason string
	}{
		{"2026-03-11 10:30", true, "office-hours"},
		{"2026-03-13 14:01", false, "friday-afternoon"},
		{"2026-03-14 11:00", false, reasonNoMatch},
		{"2026-12-23 10:00", false, "christmas-freeze"},
	}

	for _, tc := range tests {
		t.Run(tc.now, func(t *testing.T) {
			got, err := cfg.Decide(amsterdam(t, tc.now))
			if err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			if got.Allowed != tc.wantAllow || got.Reason != tc.wantReason {
				t.Errorf("Decide() = {allowed:%t reason:%q}, want {allowed:%t reason:%q}",
					got.Allowed, got.Reason, tc.wantAllow, tc.wantReason)
			}
		})
	}
}

func TestWriteOutput(t *testing.T) {
	var buf bytes.Buffer
	writeOutput(&buf, Decision{Allowed: false, Reason: "christmas\nfreeze"})

	want := "allowed=false\nreason=christmas freeze\n"
	if buf.String() != want {
		t.Errorf("writeOutput() = %q, want %q", buf.String(), want)
	}
}

func TestOverrideRequested(t *testing.T) {
	tests := map[string]bool{
		"":      false,
		" ":     false,
		"0":     false,
		"false": false,
		"FALSE": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"yes":   true,
		"ship":  true,
	}
	for value, want := range tests {
		if got := overrideRequested(value); got != want {
			t.Errorf("overrideRequested(%q) = %t, want %t", value, got, want)
		}
	}
}
