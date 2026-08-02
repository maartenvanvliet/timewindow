package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
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
	path := filepath.Join(t.TempDir(), "timewindow.yml")
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

			got, err := Evaluate(path, amsterdam(t, tc.now))
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

			got, err := Evaluate(path, time.Now())
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
	_, err := Evaluate(filepath.Join(t.TempDir(), "absent.yml"), time.Now())
	if err == nil {
		t.Fatal("Evaluate() with a missing config = nil error, want an error")
	}
}

// TestShippedConfig guards the example policy committed at timewindow.yml,
// which is also the default -config path.
func TestShippedConfig(t *testing.T) {
	cfg, err := LoadConfig(defaultConfigPath)
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
	tests := []struct {
		name     string
		decision Decision
		format   Format
		want     string
	}{
		{
			name:     "key-value",
			decision: Decision{Allowed: true, Reason: "office-hours"},
			format:   FormatKeyValue,
			want:     "allowed=true\nreason=office-hours\n",
		},
		{
			name:     "key-value keeps one record per line",
			decision: Decision{Reason: "christmas\nfreeze"},
			format:   FormatKeyValue,
			want:     "allowed=false\nreason=christmas freeze\n",
		},
		{
			name:     "json",
			decision: Decision{Allowed: false, Reason: "friday-afternoon"},
			format:   FormatJSON,
			want:     `{"allowed":false,"reason":"friday-afternoon"}` + "\n",
		},
		{
			name:     "text",
			decision: Decision{Allowed: true, Reason: "office-hours"},
			format:   FormatText,
			want:     "allowed (office-hours)\n",
		},
		{
			name:     "text when denied",
			decision: Decision{Allowed: false, Reason: "christmas-freeze"},
			format:   FormatText,
			want:     "denied (christmas-freeze)\n",
		},
		{
			name:     "none writes nothing",
			decision: Decision{Allowed: true, Reason: "office-hours"},
			format:   FormatNone,
			want:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeOutput(&buf, tc.decision, tc.format); err != nil {
				t.Fatalf("writeOutput() error = %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("writeOutput() = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	for _, in := range []string{"none", "key-value", "KEY-VALUE", " json ", "text"} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q) error = %v", in, err)
		}
	}
	for _, in := range []string{"yaml", ""} {
		if _, err := ParseFormat(in); err == nil {
			t.Errorf("ParseFormat(%q) = nil error, want an error", in)
		}
	}
}

func TestEnvName(t *testing.T) {
	for flagName, want := range map[string]string{
		"config": "TIMEWINDOW_CONFIG",
		"at":     "TIMEWINDOW_AT",
		"format": "TIMEWINDOW_FORMAT",
	} {
		if got := envName(flagName); got != want {
			t.Errorf("envName(%q) = %q, want %q", flagName, got, want)
		}
	}
}

func TestResolveTime(t *testing.T) {
	got, err := resolveTime("2026-12-24T10:00:00+01:00")
	if err != nil {
		t.Fatalf("resolveTime() error = %v", err)
	}
	if want := time.Date(2026, 12, 24, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("resolveTime() = %v, want %v", got, want)
	}

	before := time.Now()
	got, err = resolveTime("")
	if err != nil {
		t.Fatalf("resolveTime(\"\") error = %v", err)
	}
	if got.Before(before) {
		t.Errorf("resolveTime(\"\") = %v, want a time at or after %v", got, before)
	}

	if _, err := resolveTime("christmas"); err == nil {
		t.Error("resolveTime(\"christmas\") = nil error, want an error")
	}
}

// envFrom turns a map into an EnvLookup, so tests never touch the real
// environment and can run in parallel.
func envFrom(vars map[string]string) EnvLookup {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

// TestRun drives the CLI the way a caller does: arguments in, streams and an
// exit code out.
func TestRun(t *testing.T) {
	policy := writeConfig(t, officeConfig)

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr string // substring
	}{
		{
			name:     "allowed, and silent by default",
			args:     []string{"-config", policy, "-at", "2026-03-11T10:30:00+01:00"},
			wantCode: exitAllowed,
		},
		{
			name:     "denied, and silent by default",
			args:     []string{"-config", policy, "-at", "2026-03-13T14:01:00+01:00"},
			wantCode: exitDenied,
		},
		{
			name:       "key-value",
			args:       []string{"-config", policy, "-at", "2026-03-13T14:01:00+01:00", "-format", "key-value"},
			wantCode:   exitDenied,
			wantStdout: "allowed=false\nreason=friday-afternoon\n",
		},
		{
			name:       "json",
			args:       []string{"-config", policy, "-at", "2026-03-11T10:30:00+01:00", "-format", "json"},
			wantCode:   exitAllowed,
			wantStdout: `{"allowed":true,"reason":"office-hours"}` + "\n",
		},
		{
			name:       "text",
			args:       []string{"-config", policy, "-at", "2026-03-11T10:30:00+01:00", "-format", "text"},
			wantCode:   exitAllowed,
			wantStdout: "allowed (office-hours)\n",
		},
		{
			name:       "an explicit none is still silent",
			args:       []string{"-config", policy, "-at", "2026-03-11T10:30:00+01:00", "-format", "none"},
			wantCode:   exitAllowed,
			wantStdout: "",
		},

		// Environment.
		{
			name: "every flag can come from the environment",
			env: map[string]string{
				"TIMEWINDOW_CONFIG": policy,
				"TIMEWINDOW_AT":     "2026-03-11T10:30:00+01:00",
				"TIMEWINDOW_FORMAT": "text",
			},
			wantCode:   exitAllowed,
			wantStdout: "allowed (office-hours)\n",
		},
		{
			name: "a flag wins over its variable",
			args: []string{"-format", "json"},
			env: map[string]string{
				"TIMEWINDOW_CONFIG": policy,
				"TIMEWINDOW_AT":     "2026-03-11T10:30:00+01:00",
				"TIMEWINDOW_FORMAT": "text",
			},
			wantCode:   exitAllowed,
			wantStdout: `{"allowed":true,"reason":"office-hours"}` + "\n",
		},
		{
			name:       "an unusable variable is an error",
			args:       []string{"-config", policy},
			env:        map[string]string{"TIMEWINDOW_FORMAT": "yaml"},
			wantCode:   exitError,
			wantStderr: "-format:",
		},
		{
			name:       "an unrelated variable is ignored",
			args:       []string{"-config", policy, "-at", "2026-03-11T10:30:00+01:00"},
			env:        map[string]string{"TIMEWINDOW_NOPE": "x", "FORMAT": "json"},
			wantCode:   exitAllowed,
			wantStdout: "",
		},
		{
			name:       "a missing policy is an error, not a denial",
			args:       []string{"-config", filepath.Join(t.TempDir(), "absent.yml")},
			wantCode:   exitError,
			wantStderr: "timewindow: reading config",
		},
		{
			name:       "an unusable -at is an error",
			args:       []string{"-config", policy, "-at", "christmas"},
			wantCode:   exitError,
			wantStderr: "-at:",
		},
		{
			name:       "an unusable -format is an error",
			args:       []string{"-config", policy, "-format", "yaml"},
			wantCode:   exitError,
			wantStderr: "-format:",
		},
		{
			name:       "-version is not driven by the environment",
			args:       []string{"-config", policy, "-at", "2026-03-11T10:30:00+01:00"},
			env:        map[string]string{"TIMEWINDOW_VERSION": "true"},
			wantCode:   exitAllowed,
			wantStdout: "",
		},
		{
			name:       "an unknown flag is an error",
			args:       []string{"-nope"},
			wantCode:   exitError,
			wantStderr: "not defined",
		},
		{
			name:     "help is not an error",
			args:     []string{"-h"},
			wantCode: exitAllowed,
			// Usage goes to stderr, leaving stdout for the decision.
			wantStderr: "Exit codes:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(tc.args, envFrom(tc.env), &stdout, &stderr)

			if code != tc.wantCode {
				t.Errorf("run() = %d, want %d (stderr: %s)", code, tc.wantCode, stderr.String())
			}
			if stdout.String() != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantStderr == "" && stderr.Len() > 0 {
				t.Errorf("stderr = %q, want it empty", stderr.String())
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"-version"}, envFrom(nil), &stdout, &stderr); code != exitAllowed {
		t.Errorf("run(-version) = %d, want %d", code, exitAllowed)
	}
	if !strings.HasPrefix(stdout.String(), "timewindow ") {
		t.Errorf("stdout = %q, want it to start with the program name", stdout.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("stderr = %q, want it empty", stderr.String())
	}
}

// TestParseConfigFromBytes covers the path -config - uses, where the policy
// never touches the filesystem.
func TestParseConfigFromBytes(t *testing.T) {
	cfg, err := ParseConfig([]byte(officeConfig), "stdin")
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}

	got, err := cfg.Decide(amsterdam(t, "2026-03-11 10:30"))
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if !got.Allowed || got.Reason != "office-hours" {
		t.Errorf("Decide() = %+v, want allowed by office-hours", got)
	}

	if _, err := ParseConfig([]byte("rules: ["), "stdin"); err == nil {
		t.Error("ParseConfig() with broken YAML = nil error, want an error")
	}
}

func TestResolveBuildInfo(t *testing.T) {
	stamped := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.9.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
			{Key: "vcs.time", Value: "2026-08-01T09:00:00Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}

	tests := []struct {
		name                  string
		version, commit, date string
		stamped               *debug.BuildInfo
		ok                    bool
		want                  BuildInfo
	}{
		{
			name:    "ldflags win over the toolchain stamps",
			version: "v1.2.3", commit: "abcdef123456", date: "2026-08-02T10:00:00Z",
			stamped: stamped, ok: true,
			want: BuildInfo{Version: "v1.2.3", Commit: "abcdef123456", Date: "2026-08-02T10:00:00Z"},
		},
		{
			name:    "an injected full sha is trimmed for display",
			version: "v1.2.3", commit: "0123456789abcdef0123456789abcdef01234567", date: "2026-08-02T10:00:00Z",
			stamped: stamped, ok: true,
			want: BuildInfo{Version: "v1.2.3", Commit: "0123456789ab", Date: "2026-08-02T10:00:00Z"},
		},
		{
			name:    "a go install build describes itself from the stamps",
			version: "dev",
			stamped: stamped, ok: true,
			want: BuildInfo{Version: "v0.9.0", Commit: "0123456789ab", Date: "2026-08-01T09:00:00Z"},
		},
		{
			name:    "a dirty tree is flagged",
			version: "dev",
			stamped: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef"},
				{Key: "vcs.modified", Value: "true"},
			}},
			ok:   true,
			want: BuildInfo{Version: "dev", Commit: "0123456789ab", Dirty: true},
		},
		{
			name:    "no build info at all",
			version: "dev",
			stamped: nil, ok: false,
			want: BuildInfo{Version: "dev"},
		},
		{
			name:    "an unstamped devel build stays dev",
			version: "dev",
			stamped: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, ok: true,
			want: BuildInfo{Version: "dev"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBuildInfo(tc.version, tc.commit, tc.date, tc.stamped, tc.ok)
			if got != tc.want {
				t.Errorf("resolveBuildInfo() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBuildInfoString(t *testing.T) {
	got := BuildInfo{Version: "v1.2.3", Commit: "abcdef123456", Date: "2026-08-02T10:00:00Z"}.String()
	for _, want := range []string{"timewindow v1.2.3", "commit: abcdef123456", "built:  2026-08-02T10:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}

	// Gaps are filled in rather than printed as empty fields.
	got = BuildInfo{Dirty: true}.String()
	for _, want := range []string{"timewindow dev", "commit: unknown-dirty", "built:  unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}

func TestApplyEnv(t *testing.T) {
	newFlags := func() (*flag.FlagSet, *string) {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		value := flags.String("format", "none", "")
		return flags, value
	}

	t.Run("fills in a flag that was not given", func(t *testing.T) {
		flags, format := newFlags()
		if err := flags.Parse(nil); err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if err := applyEnv(flags, envFrom(map[string]string{"TIMEWINDOW_FORMAT": "json"})); err != nil {
			t.Fatalf("applyEnv() error = %v", err)
		}
		if *format != "json" {
			t.Errorf("format = %q, want %q", *format, "json")
		}
	})

	t.Run("leaves a flag that was given", func(t *testing.T) {
		flags, format := newFlags()
		if err := flags.Parse([]string{"-format", "text"}); err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if err := applyEnv(flags, envFrom(map[string]string{"TIMEWINDOW_FORMAT": "json"})); err != nil {
			t.Fatalf("applyEnv() error = %v", err)
		}
		if *format != "text" {
			t.Errorf("format = %q, want the flag value %q", *format, "text")
		}
	})

	t.Run("an empty variable is still a value", func(t *testing.T) {
		flags, format := newFlags()
		if err := flags.Parse(nil); err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if err := applyEnv(flags, envFrom(map[string]string{"TIMEWINDOW_FORMAT": ""})); err != nil {
			t.Fatalf("applyEnv() error = %v", err)
		}
		if *format != "" {
			t.Errorf("format = %q, want it emptied by the variable", *format)
		}
	})

	t.Run("no lookup is not an error", func(t *testing.T) {
		flags, _ := newFlags()
		if err := applyEnv(flags, nil); err != nil {
			t.Errorf("applyEnv(nil) error = %v", err)
		}
	})
}
