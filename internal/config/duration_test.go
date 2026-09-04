package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationAcceptsStringsAndSeconds(t *testing.T) {
	var cfg struct {
		A Duration `yaml:"a"`
		B Duration `yaml:"b"`
		C Duration `yaml:"c"`
		D Duration `yaml:"d"`
	}
	// The bare `0` is the case that motivated this type: plain time.Duration rejects
	// it with a confusing type error.
	body := "a: 5s\nb: 0\nc: 90\nd: 2m30s\n"

	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		got  Duration
		want time.Duration
	}{
		{"a", cfg.A, 5 * time.Second},
		{"b", cfg.B, 0},
		{"c", cfg.C, 90 * time.Second},
		{"d", cfg.D, 150 * time.Second},
	} {
		if tc.got.D() != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got.D(), tc.want)
		}
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var cfg struct {
		A Duration `yaml:"a"`
	}
	if err := yaml.Unmarshal([]byte("a: not-a-duration\n"), &cfg); err == nil {
		t.Error("expected an error for an unparseable duration")
	}
}

func TestDurationRoundTrips(t *testing.T) {
	in := Duration(90 * time.Second)
	out, err := yaml.Marshal(map[string]Duration{"x": in})
	if err != nil {
		t.Fatal(err)
	}

	var back struct {
		X Duration `yaml:"x"`
	}
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.X.D() != in.D() {
		t.Errorf("round trip: %v -> %s -> %v", in.D(), out, back.X.D())
	}
}
