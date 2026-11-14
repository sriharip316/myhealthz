package config

import "testing"

func TestParseVersionFlag(t *testing.T) {
	cfg, err := Parse([]string{"-version"})
	if err != nil {
		t.Fatalf("Parse(-version) returned error: %v", err)
	}
	if !cfg.Version {
		t.Fatal("cfg.Version = false, want true")
	}
}

func TestParseVersionDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil) returned error: %v", err)
	}
	if cfg.Version {
		t.Fatal("cfg.Version = true, want false when -version is absent")
	}
}
