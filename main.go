// Command deploy-gate decides whether a production deployment is allowed to
// run right now, based on a YAML config that reuses Prometheus Alertmanager's
// time_intervals schema.
//
// It prints two KEY=value lines to stdout so the caller can redirect straight
// into $GITHUB_OUTPUT, and signals the decision through its exit code:
//
//	0 - deployment allowed
//	1 - deployment denied by the configured windows
//	2 - config could not be read, parsed or validated
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
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

	// overrideEnvVar lets an operator force a deployment through a closed
	// window. Any value other than the empty string, "0", "false" or "no"
	// (case-insensitive) counts as set.
	overrideEnvVar = "DEPLOY_GATE_OVERRIDE"

	exitAllowed     = 0
	exitDenied      = 1
	exitConfigError = 2
)

// Config is the outer document. The intervals themselves are parsed by
// alertmanager's own YAML unmarshaling, so the accepted syntax for weekdays,
// months, days_of_month, years, times and location is identical to
// Alertmanager's.
type Config struct {
	// Timezone is the default location for intervals that do not carry
	// their own `location` key.
	Timezone string `yaml:"timezone"`

	TimeIntervals []NamedTimeInterval `yaml:"time_intervals"`

	// ActiveTimeIntervals lists the windows during which deploying is
	// allowed. Empty means "no window restriction".
	ActiveTimeIntervals []string `yaml:"active_time_intervals"`

	// MuteTimeIntervals lists the windows during which deploying is
	// blocked. A mute match always wins over an active match.
	MuteTimeIntervals []string `yaml:"mute_time_intervals"`
}

// NamedTimeInterval mirrors Alertmanager's named time interval: a name plus a
// list of intervals that are OR'ed together.
type NamedTimeInterval struct {
	Name          string                      `yaml:"name"`
	TimeIntervals []timeinterval.TimeInterval `yaml:"time_intervals"`
}

// Decision is the outcome of evaluating a config at a point in time.
type Decision struct {
	Allowed bool
	Reason  string
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to the deploy window config file")
	flag.Parse()

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
	// Deliberately not UnmarshalStrict: Alertmanager-flavoured configs in
	// the wild carry annotation keys (for example `Action: deny`) that are
	// not part of the schema, and rejecting them would be surprising.
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := cfg.applyTimezone(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyTimezone resolves the top-level timezone and uses it as the default
// location for every interval that does not set `location` itself.
func (c *Config) applyTimezone() error {
	if c.Timezone == "" {
		return nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	for _, named := range c.TimeIntervals {
		for i := range named.TimeIntervals {
			if named.TimeIntervals[i].Location == nil {
				named.TimeIntervals[i].Location = &timeinterval.Location{Location: loc}
			}
		}
	}
	return nil
}

// validate rejects configs that would otherwise produce a silent or
// misleading decision.
func (c *Config) validate() error {
	seen := make(map[string]bool, len(c.TimeIntervals))
	for _, named := range c.TimeIntervals {
		if named.Name == "" {
			return fmt.Errorf("time_intervals: entry with an empty name")
		}
		if seen[named.Name] {
			return fmt.Errorf("time_intervals: duplicate interval name %q", named.Name)
		}
		seen[named.Name] = true
	}

	for _, ref := range c.ActiveTimeIntervals {
		if !seen[ref] {
			return fmt.Errorf("active_time_intervals references undefined time interval %q", ref)
		}
	}
	for _, ref := range c.MuteTimeIntervals {
		if !seen[ref] {
			return fmt.Errorf("mute_time_intervals references undefined time interval %q", ref)
		}
	}
	return nil
}

// Decide applies the deploy policy at the given time. Deploying is allowed
// when no active window is configured or one of them matches, and no mute
// window matches.
func (c *Config) Decide(now time.Time) (Decision, error) {
	intervener := timeinterval.NewIntervener(c.intervalsByName())

	muted, mutedBy, err := intervener.Mutes(c.MuteTimeIntervals, now)
	if err != nil {
		return Decision{}, fmt.Errorf("evaluating mute_time_intervals: %w", err)
	}
	if muted {
		return Decision{Allowed: false, Reason: join(mutedBy)}, nil
	}

	if len(c.ActiveTimeIntervals) == 0 {
		return Decision{Allowed: true, Reason: "no deploy window configured"}, nil
	}

	inWindow, activeIn, err := intervener.Mutes(c.ActiveTimeIntervals, now)
	if err != nil {
		return Decision{}, fmt.Errorf("evaluating active_time_intervals: %w", err)
	}
	if !inWindow {
		return Decision{Allowed: false, Reason: "no active window matched"}, nil
	}
	return Decision{Allowed: true, Reason: join(activeIn)}, nil
}

// intervalsByName shapes the config for timeinterval.NewIntervener.
func (c *Config) intervalsByName() map[string][]timeinterval.TimeInterval {
	byName := make(map[string][]timeinterval.TimeInterval, len(c.TimeIntervals))
	for _, named := range c.TimeIntervals {
		byName[named.Name] = named.TimeIntervals
	}
	return byName
}

// join renders the matched interval names deterministically; Mutes can report
// the same name once per matching sub-interval.
func join(names []string) string {
	unique := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			unique = append(unique, n)
		}
	}
	sort.Strings(unique)
	return strings.Join(unique, ",")
}
