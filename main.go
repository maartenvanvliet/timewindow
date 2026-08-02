// Command deploy-gate decides whether a production deployment is allowed to
// run right now, by evaluating a list of rules against the current time.
//
// Rules work like firewall rules: they are evaluated top to bottom and the
// last one that matches wins, so a config can layer broad allows first and
// narrower denies after them. Each rule matches on Prometheus Alertmanager's
// time_intervals schema.
//
// The tool prints two KEY=value lines to stdout so the caller can redirect
// straight into $GITHUB_OUTPUT, and signals the decision through its exit
// code:
//
//	0 - deployment allowed
//	1 - deployment denied
//	2 - config could not be read, parsed or validated
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/prometheus/alertmanager/timeinterval"
	"gopkg.in/yaml.v2"

	// Embed the IANA database so location names such as Europe/Amsterdam
	// resolve on minimal CI images that ship without /usr/share/zoneinfo.
	_ "time/tzdata"
)

const (
	// defaultConfigPath is used when -config is not given, so the workflow
	// step can be a bare `go run .`.
	defaultConfigPath = ".github/deploy-window.yml"

	// overrideEnvVar lets an operator force a deployment past the rules.
	// Any value other than the empty string, "0", "false" or "no"
	// (case-insensitive) counts as set.
	overrideEnvVar = "DEPLOY_GATE_OVERRIDE"

	// reasonNoMatch is reported when no rule matched and the default
	// action decided the outcome.
	reasonNoMatch = "no rule matched"

	exitAllowed     = 0
	exitDenied      = 1
	exitConfigError = 2
)

// Build metadata. Release binaries set these with -ldflags -X; other builds
// fall back to whatever the Go toolchain stamped into the binary.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
	Dirty   bool
}

// resolveBuildInfo prefers values injected at link time and fills the gaps
// from the toolchain's own VCS stamps, so `go build` and `go install` binaries
// still identify themselves.
func resolveBuildInfo(version, commit, date string, bi *debug.BuildInfo, ok bool) BuildInfo {
	out := BuildInfo{Version: version, Commit: commit, Date: date}
	if !ok || bi == nil {
		return out
	}

	if (out.Version == "" || out.Version == "dev") && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		out.Version = bi.Main.Version
	}
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			if out.Commit == "" {
				out.Commit = shortCommit(setting.Value)
			}
		case "vcs.time":
			if out.Date == "" {
				out.Date = setting.Value
			}
		case "vcs.modified":
			out.Dirty = setting.Value == "true"
		}
	}
	return out
}

// shortCommit trims a full SHA down to the usual display length.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// String renders the -version output.
func (b BuildInfo) String() string {
	version := b.Version
	if version == "" {
		version = "dev"
	}
	commit := b.Commit
	if commit == "" {
		commit = "unknown"
	}
	if b.Dirty {
		commit += "-dirty"
	}
	date := b.Date
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("deploy-gate %s\ncommit: %s\nbuilt:  %s\ngo:     %s %s/%s",
		version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Action is what a rule does when it matches.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
)

// parseAction validates an action written in the config.
func parseAction(s string) (Action, error) {
	switch Action(strings.ToLower(strings.TrimSpace(s))) {
	case ActionAllow:
		return ActionAllow, nil
	case ActionDeny:
		return ActionDeny, nil
	default:
		return "", fmt.Errorf("action must be %q or %q, got %q", ActionAllow, ActionDeny, s)
	}
}

// Config is the outer document. Interval bodies are parsed by alertmanager's
// own YAML unmarshaling, so the accepted syntax for weekdays, months,
// days_of_month, years, times and location is identical to Alertmanager's.
type Config struct {
	// Timezone is the default location for intervals that do not carry
	// their own `location` key.
	Timezone string `yaml:"timezone"`

	// Default is the action to take when no rule matches. Optional;
	// "deny" when omitted, so an empty or partial policy fails closed.
	Default string `yaml:"default"`

	// Rules are evaluated in order; the last match wins.
	Rules []Rule `yaml:"rules"`

	// defaultAction is Default, resolved during loading.
	defaultAction Action `yaml:"-"`

	// Schema from before the rule engine, kept only so a stale config gets
	// a migration error instead of being silently ignored.
	LegacyTimeIntervals []yaml.MapSlice `yaml:"time_intervals"`
	LegacyActive        []string        `yaml:"active_time_intervals"`
	LegacyMute          []string        `yaml:"mute_time_intervals"`
}

// Rule is one entry in the evaluation chain: a set of time intervals and what
// to do when the current time falls inside any of them.
type Rule struct {
	// Name identifies the rule in the reported reason. Optional; a rule
	// without one is called rule[i] after its position.
	Name string `yaml:"name"`

	// Action is "allow" or "deny". Required.
	Action string `yaml:"action"`

	// TimeIntervals are OR'ed together: the rule matches when the current
	// time falls in any of them. A single empty interval ({}) matches
	// every time, which is how you write an unconditional base rule.
	TimeIntervals []RuleInterval `yaml:"time_intervals"`

	// name and action are Name and Action, resolved during loading.
	name   string `yaml:"-"`
	action Action `yaml:"-"`
}

// RuleInterval is an Alertmanager time interval. It exists only to catch an
// `action` written one level too deep; the action belongs on the rule, since
// the rule is the unit that matches.
type RuleInterval struct {
	timeinterval.TimeInterval `yaml:",inline"`

	StrayAction      string `yaml:"action"`
	StrayActionUpper string `yaml:"Action"`
}

// Decision is the outcome of evaluating a config at a point in time.
type Decision struct {
	Allowed bool
	Reason  string
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to the deploy window config file")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		stamped, ok := debug.ReadBuildInfo()
		fmt.Fprintln(os.Stdout, resolveBuildInfo(version, commit, date, stamped, ok))
		os.Exit(exitAllowed)
	}

	decision, err := Evaluate(*configPath, time.Now(), os.Getenv(overrideEnvVar))
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy-gate: %v\n", err)
		os.Exit(exitConfigError)
	}

	writeOutput(os.Stdout, decision)
	if !decision.Allowed {
		os.Exit(exitDenied)
	}
	os.Exit(exitAllowed)
}

// usage documents the exit codes, which are the part of this CLI a caller is
// most likely to get wrong.
func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `deploy-gate decides whether a production deployment may run right now.

Usage:
  deploy-gate [-config path]

It prints two KEY=value lines on stdout, ready for $GITHUB_OUTPUT:

  allowed=true|false
  reason=<name of the deciding rule | "manual override" | "no rule matched">

Exit codes:
  0  allowed
  1  denied
  2  the config is missing, unparseable or invalid (error on stderr)

Set `+overrideEnvVar+` to a value other than 0/false/no to force a deploy.

Flags:
`)
	flag.PrintDefaults()
}

// writeOutput emits the decision as $GITHUB_OUTPUT-compatible KEY=value lines.
func writeOutput(w io.Writer, d Decision) {
	fmt.Fprintf(w, "allowed=%t\n", d.Allowed)
	fmt.Fprintf(w, "reason=%s\n", sanitize(d.Reason))
}

// sanitize keeps a reason on a single line; a multi-line value would break the
// KEY=value protocol GitHub Actions expects.
func sanitize(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// Evaluate loads the config at path and decides whether deploying is allowed
// at the given time. A non-nil error means the config is unusable; callers
// must never treat that as "allowed".
func Evaluate(path string, now time.Time, override string) (Decision, error) {
	if overrideRequested(override) {
		return Decision{Allowed: true, Reason: "manual override"}, nil
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		return Decision{}, err
	}
	return cfg.Decide(now)
}

// overrideRequested reports whether the override env var value asks for a
// forced deployment.
func overrideRequested(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}

// LoadConfig reads, parses and validates a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	// Deliberately not UnmarshalStrict: unknown keys elsewhere in the
	// document are tolerated. The keys that would actually change a
	// decision if ignored are caught explicitly in resolve().
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := cfg.resolve(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// resolve fills in the derived fields and rejects configs that would otherwise
// produce a silent or misleading decision.
func (c *Config) resolve() error {
	if err := c.rejectLegacySchema(); err != nil {
		return err
	}
	if err := c.resolveDefault(); err != nil {
		return err
	}
	if err := c.resolveTimezone(); err != nil {
		return err
	}
	return c.resolveRules()
}

// rejectLegacySchema turns a config written for the pre-rules schema into an
// explicit error, rather than an empty rule chain that denies everything.
func (c *Config) rejectLegacySchema() error {
	var stale []string
	if len(c.LegacyTimeIntervals) > 0 {
		stale = append(stale, "time_intervals")
	}
	if len(c.LegacyActive) > 0 {
		stale = append(stale, "active_time_intervals")
	}
	if len(c.LegacyMute) > 0 {
		stale = append(stale, "mute_time_intervals")
	}
	if len(stale) > 0 {
		return fmt.Errorf("top-level %s is no longer supported: define `rules` with an `action` each instead",
			strings.Join(stale, ", "))
	}
	return nil
}

// resolveDefault picks the action used when no rule matches.
func (c *Config) resolveDefault() error {
	if c.Default == "" {
		c.defaultAction = ActionDeny
		return nil
	}
	action, err := parseAction(c.Default)
	if err != nil {
		return fmt.Errorf("default: %w", err)
	}
	c.defaultAction = action
	return nil
}

// resolveTimezone resolves the top-level timezone and uses it as the default
// location for every interval that does not set `location` itself.
func (c *Config) resolveTimezone() error {
	if c.Timezone == "" {
		return nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	for _, rule := range c.Rules {
		for i := range rule.TimeIntervals {
			if rule.TimeIntervals[i].Location == nil {
				rule.TimeIntervals[i].Location = &timeinterval.Location{Location: loc}
			}
		}
	}
	return nil
}

// resolveRules names every rule, validates its action and intervals, and
// rejects duplicate names.
func (c *Config) resolveRules() error {
	seen := make(map[string]bool, len(c.Rules))

	for i := range c.Rules {
		rule := &c.Rules[i]

		rule.name = rule.Name
		if rule.name == "" {
			rule.name = fmt.Sprintf("rule[%d]", i)
		}
		if seen[rule.name] {
			return fmt.Errorf("rules: duplicate rule name %q", rule.name)
		}
		seen[rule.name] = true

		action, err := parseAction(rule.Action)
		if err != nil {
			return fmt.Errorf("rule %q: %w", rule.name, err)
		}
		rule.action = action

		if len(rule.TimeIntervals) == 0 {
			return fmt.Errorf("rule %q: no time_intervals defined, so it can never match "+
				"(use `time_intervals: [{}]` for a rule that always matches)", rule.name)
		}
		for _, interval := range rule.TimeIntervals {
			if interval.StrayAction != "" || interval.StrayActionUpper != "" {
				return fmt.Errorf("rule %q: `action` belongs on the rule, not on an entry "+
					"under its time_intervals", rule.name)
			}
		}
	}
	return nil
}

// Decide runs the rule chain at the given time. Rules are evaluated in order
// and the last match wins; when nothing matches, the default action decides.
func (c *Config) Decide(now time.Time) (Decision, error) {
	intervener := timeinterval.NewIntervener(c.intervalsByName())

	action, reason := c.defaultAction, reasonNoMatch
	for _, rule := range c.Rules {
		matched, _, err := intervener.Mutes([]string{rule.name}, now)
		if err != nil {
			return Decision{}, fmt.Errorf("evaluating rule %q: %w", rule.name, err)
		}
		if matched {
			action, reason = rule.action, rule.name
		}
	}

	return Decision{Allowed: action == ActionAllow, Reason: reason}, nil
}

// intervalsByName shapes the rules for timeinterval.NewIntervener, keyed by
// the resolved rule name.
func (c *Config) intervalsByName() map[string][]timeinterval.TimeInterval {
	byName := make(map[string][]timeinterval.TimeInterval, len(c.Rules))
	for _, rule := range c.Rules {
		intervals := make([]timeinterval.TimeInterval, 0, len(rule.TimeIntervals))
		for _, interval := range rule.TimeIntervals {
			intervals = append(intervals, interval.TimeInterval)
		}
		byName[rule.name] = intervals
	}
	return byName
}
