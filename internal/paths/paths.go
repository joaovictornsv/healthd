// Package paths resolves where healthd keeps its config, state and socket,
// following the XDG base directory spec.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const appName = "healthd"

func ConfigFile() string {
	return filepath.Join(xdgDir("XDG_CONFIG_HOME", ".config"), appName, "config.toml")
}

func StateDir() string {
	return filepath.Join(xdgDir("XDG_STATE_HOME", ".local/state"), appName)
}

func HistoryDB() string {
	return filepath.Join(StateDir(), "history.db")
}

func Socket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, appName+".sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.sock", appName, os.Getuid()))
}

func xdgDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, fallback)
}
