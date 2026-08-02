// Command timewindow reports whether a point in time is allowed by a policy
// of time-based rules.
//
// Rules work like firewall rules: they are evaluated top to bottom and the
// last one that matches wins, so a policy can layer broad allows first and
// narrower denies after them. Each rule matches on Prometheus Alertmanager's
// time_intervals schema.
//
// The decision goes to stdout and to the exit status, so it can gate anything
// a shell can gate:
//
//	timewindow -quiet || exit 0
//
// Exit codes:
//
//	0 - allowed
//	1 - denied
//	2 - the policy or the arguments could not be used
package main

import (
	"encoding/json"
	"errors"
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
	// programName is used for the usage text, the version banner and the
	// error prefix.
	programName = "timewindow"

	// defaultConfigPath is where the policy is read from when -config is
	// not given.
	defaultConfigPath = "timewindow.yml"

	// reasonNoMatch is reported when no rule matched and the default
	// action decided the outcome.
	reasonNoMatch = "no rule matched"

	exitAllowed = 0
	exitDenied  = 1
	// exitError covers an unusable policy and unusable arguments alike:
	// either way the tool could not answer the question.
	exitError = 2
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
	out := BuildInfo{Version: version, Commit: shortCommit(commit), Date: date}
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

// shortCommit trims a full SHA down to the usual display length, wherever it
// came from: a release injects the full one.
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
	return fmt.Sprintf("%s %s\ncommit: %s\nbuilt:  %s\ngo:     %s %s/%s",
		programName, version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
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
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole CLI, so that argument handling, output and exit codes can
// be exercised in tests the same way a caller sees them.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(programName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr, flags) }

	configPath := flags.String("config", defaultConfigPath, "path to the policy file, or - for stdin")
	at := flags.String("at", "", "evaluate at this RFC 3339 time instead of now")
	format := flags.String("format", string(FormatKeyValue), "output format: key-value or json")
	quiet := flags.Bool("quiet", false, "print nothing; report the decision through the exit status only")
	showVersion := flags.Bool("version", false, "print version information and exit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitAllowed
		}
		return exitError
	}

	if *showVersion {
		stamped, ok := debug.ReadBuildInfo()
		fmt.Fprintln(stdout, resolveBuildInfo(version, commit, date, stamped, ok))
		return exitAllowed
	}

	decision, err := evaluate(*configPath, *at, *format, stdout, *quiet)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitError
	}
	if !decision.Allowed {
		return exitDenied
	}
	return exitAllowed
}

// evaluate resolves the arguments, makes the decision and reports it.
func evaluate(configPath, at, format string, stdout io.Writer, quiet bool) (Decision, error) {
	when, err := resolveTime(at)
	if err != nil {
		return Decision{}, err
	}
	outputFormat, err := ParseFormat(format)
	if err != nil {
		return Decision{}, err
	}

	decision, err := Evaluate(configPath, when)
	if err != nil {
		return Decision{}, err
	}

	if !quiet {
		if err := writeOutput(stdout, decision, outputFormat); err != nil {
			return Decision{}, err
		}
	}
	return decision, nil
}

// resolveTime turns the -at flag into the instant to evaluate.
func resolveTime(at string) (time.Time, error) {
	if at == "" {
		return time.Now(), nil
	}
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, fmt.Errorf("-at: %q is not an RFC 3339 time such as 2026-12-24T10:00:00+01:00", at)
	}
	return when, nil
}

// usage keeps the exit codes in front of the reader, since they are the part
// of this tool a caller is most likely to get wrong.
func usage(w io.Writer, flags *flag.FlagSet) {
	fmt.Fprintf(w, `%s reports whether a point in time is allowed by a policy of
time-based rules. Rules are evaluated top to bottom and the last match wins.

Usage:
  %s [flags]

Exit codes:
  0  allowed
  1  denied
  2  the policy or the arguments could not be used (message on stderr)

The exit status is the whole answer, so the decision can gate a shell:

  %s -quiet && ./release.sh

Flags:
`, programName, programName, programName)
	flags.PrintDefaults()
}

// Format is how a decision is rendered on stdout.
type Format string

const (
	// FormatKeyValue writes `key=value` lines. It suits shell `eval` and
	// appending to a CI step's output file.
	FormatKeyValue Format = "key-value"

	// FormatJSON writes one JSON object.
	FormatJSON Format = "json"
)

// ParseFormat validates an output format name.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatKeyValue:
		return FormatKeyValue, nil
	case FormatJSON:
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("-format: must be %q or %q, got %q", FormatKeyValue, FormatJSON, s)
	}
}

// writeOutput renders the decision in the requested format.
func writeOutput(w io.Writer, d Decision, format Format) error {
	switch format {
	case FormatJSON:
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(d)
	default:
		_, err := fmt.Fprintf(w, "allowed=%t\nreason=%s\n", d.Allowed, sanitize(d.Reason))
		return err
	}
}

// sanitize keeps a reason on a single line, so one `key=value` record cannot
// spill into the next.
func sanitize(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// Evaluate loads the policy at path and decides what it says about the given
// time. A non-nil error means the policy is unusable; callers must never treat
// that as "allowed".
func Evaluate(path string, when time.Time) (Decision, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return Decision{}, err
	}
	return cfg.Decide(when)
}

// LoadConfig reads, parses and validates a policy file. A path of "-" reads
// stdin, so a policy can be piped in or generated on the fly.
func LoadConfig(path string) (*Config, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return ParseConfig(raw, path)
}

// ParseConfig parses and validates a policy document. source names it in error
// messages.
func ParseConfig(raw []byte, source string) (*Config, error) {
	var cfg Config
	// Deliberately not UnmarshalStrict: unknown keys elsewhere in the
	// document are tolerated. The keys that would actually change a
	// decision if ignored are caught explicitly in resolve().
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", source, err)
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
