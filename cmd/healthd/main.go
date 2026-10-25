// healthd is the background service: it samples machine health indicators,
// sends desktop notifications when something goes wrong, and serves the
// data to the health TUI over a unix socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/joaovictornsv/healthd/internal/daemon"
	"github.com/joaovictornsv/healthd/internal/paths"
)

const usage = `Usage: healthd [flags] [command]

Commands:
  run              run the service in the foreground (default)
  detect           print detected hardware and effective thresholds
  install-service  install and start the systemd --user service

Flags:
`

func main() {
	opts := daemon.Options{}
	flag.StringVar(&opts.ConfigPath, "config", paths.ConfigFile(), "config file")
	flag.StringVar(&opts.SocketPath, "socket", paths.Socket(), "API socket")
	flag.StringVar(&opts.DBPath, "db", paths.HistoryDB(), "history database")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	var err error
	switch cmd := flag.Arg(0); cmd {
	case "", "run":
		err = daemon.New(opts).Run(context.Background())
	case "detect":
		err = daemon.Detect(os.Stdout, opts.ConfigPath)
	case "install-service":
		err = installService()
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthd:", err)
		os.Exit(1)
	}
}

const unitTemplate = `[Unit]
Description=healthd machine health monitor
Documentation=https://github.com/joaovictornsv/healthd

[Service]
ExecStart=%s
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(configHome, "systemd/user/healthd.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(unitTemplate, exe)), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", unitPath)
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", "healthd.service"},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}
	fmt.Println("healthd is running; logs: journalctl --user -u healthd -f")
	return nil
}
