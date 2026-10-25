package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSoundPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	for _, f := range []string{filepath.Join(dir, "sounds", "fire.mp3"), filepath.Join(home, "done.ogg")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.toml")
	conf := `[notify]
firing_sound = "sounds/fire.mp3"
critical_sound = "missing.mp3"
resolved_sound = "~/done.ogg"
`
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "sounds", "fire.mp3"); cfg.Notify.FiringSound != want {
		t.Errorf("firing_sound = %q, want %q", cfg.Notify.FiringSound, want)
	}
	if want := filepath.Join(home, "done.ogg"); cfg.Notify.ResolvedSound != want {
		t.Errorf("resolved_sound = %q, want %q", cfg.Notify.ResolvedSound, want)
	}
	if cfg.Notify.CriticalSound != "" {
		t.Errorf("missing critical_sound should fall back to built-in, got %q", cfg.Notify.CriticalSound)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "critical_sound") {
		t.Errorf("warnings = %q, want one about critical_sound", warnings)
	}
}
