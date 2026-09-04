package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that accepts either a duration string ("5s", "2m") or a
// bare number of seconds in YAML.
//
// Plain time.Duration rejects `interval: 0` with a type error, which is a confusing
// thing to hit while hand-editing a config. Accepting both forms costs little and
// removes a papercut.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
//
// Dispatch is on the YAML tag rather than on whether a decode succeeds: a plain
// integer decodes into a string just fine, so trying the string form first would
// swallow `interval: 90` and fail on time.ParseDuration("90").
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	switch n.Tag {
	case "!!int", "!!float":
		var secs float64
		if err := n.Decode(&secs); err != nil {
			return fmt.Errorf("config: line %d: %w", n.Line, err)
		}
		*d = Duration(secs * float64(time.Second))
		return nil
	}

	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("config: line %d: expected a duration like \"5s\" or a number of seconds", n.Line)
	}
	if parsed, err := time.ParseDuration(s); err == nil {
		*d = Duration(parsed)
		return nil
	}
	// A quoted number ("90") is still unambiguous; treat it as seconds.
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(secs * float64(time.Second))
		return nil
	}
	return fmt.Errorf("config: line %d: %q is not a duration (try \"5s\", \"2m\" or a number of seconds)", n.Line, s)
}

// MarshalYAML emits the canonical string form.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
