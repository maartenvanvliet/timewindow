package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exampleConfig is the config from the README, verbatim in its interval
// definitions, wired to a policy that allows deploys during office hours and
// blocks them during the Christmas freeze.
const exampleConfig = `
timezone: Europe/Amsterdam

active_time_intervals: ['office-hours']
mute_time_intervals: ['christmas-freeze']

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
`

const unknownIntervalConfig = `
timezone: Europe/Amsterdam

active_time_intervals: ['office-hourz']

time_intervals:
  - name: office-hours
    time_intervals:
      - weekdays: ['monday:friday']
`

const malformedConfig = `
timezone: Europe/Amsterdam
time_intervals:
  - name: office-hours
   time_intervals:
      - weekdays: ['monday'
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
		wantErr    bool
		wantErrMsg string // substring of the error
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "within office hours on a wednesday",
			config:     exampleConfig,
			now:        "2026-03-11 10:30",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "friday before the early close",
			config:     exampleConfig,
			now:        "2026-03-13 13:59",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "friday after 14:00",
			config:     exampleConfig,
			now:        "2026-03-13 14:01",
			wantAllow:  false,
			wantReason: "no active window matched",
		},
		{
			name:       "weekend",
			config:     exampleConfig,
			now:        "2026-03-14 11:00",
			wantAllow:  false,
			wantReason: "no active window matched",
		},
		{
			name:       "inside the christmas freeze during office hours",
			config:     exampleConfig,
			now:        "2026-12-23 10:00",
			wantAllow:  false,
			wantReason: "christmas-freeze",
		},
		{
			name:       "inside the january tail of the freeze",
			config:     exampleConfig,
			now:        "2027-01-01 10:00",
			wantAllow:  false,
			wantReason: "christmas-freeze",
		},
		{
			name:       "just after the freeze ends",
			config:     exampleConfig,
			now:        "2027-01-04 10:00",
			wantAllow:  true,
			wantReason: "office-hours",
		},
		{
			name:       "override wins over a closed window",
			config:     exampleConfig,
			now:        "2026-03-14 11:00",
			override:   "true",
			wantAllow:  true,
			wantReason: "manual override",
		},
		{
			name:       "falsey override does not force a deploy",
			config:     exampleConfig,
			now:        "2026-03-14 11:00",
			override:   "false",
			wantAllow:  false,
			wantReason: "no active window matched",
		},
		{
			name:       "no windows configured allows deploying",
			config:     "timezone: Europe/Amsterdam\n",
			now:        "2026-03-14 11:00",
			wantAllow:  true,
			wantReason: "no deploy window configured",
		},
		{
			name:       "unknown interval name in policy",
			config:     unknownIntervalConfig,
			now:        "2026-03-11 10:30",
			wantErr:    true,
			wantErrMsg: `active_time_intervals references undefined time interval "office-hourz"`,
		},
		{
			name:       "malformed yaml",
			config:     malformedConfig,
			now:        "2026-03-11 10:30",
			wantErr:    true,
			wantErrMsg: "parsing config",
		},
		{
			name:       "invalid timezone",
			config:     "timezone: Mars/Olympus_Mons\n",
			now:        "2026-03-11 10:30",
			wantErr:    true,
			wantErrMsg: "invalid timezone",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.config)

			got, err := Evaluate(path, amsterdam(t, tc.now), tc.override)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Evaluate() = %+v, want error", got)
				}
				if !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErrMsg)
				}
				return
			}
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

// TestShippedConfigParses guards the config committed at .github/deploy-window.yml.
func TestShippedConfigParses(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(".github", "deploy-window.yml"))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	got, err := cfg.Decide(amsterdam(t, "2026-03-11 10:30"))
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if !got.Allowed || got.Reason != "office-hours" {
		t.Errorf("Decide() = %+v, want allowed during office-hours", got)
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
