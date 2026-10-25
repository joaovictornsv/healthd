// Package notify sends desktop notifications through the freedesktop
// notification service on the session D-Bus.
package notify

import (
	"bytes"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/joaovictornsv/healthd/internal/paths"
)

type Urgency byte

const (
	Low Urgency = iota
	Normal
	Critical
)

type Message struct {
	// Key groups notifications so a newer one replaces the previous bubble.
	Key     string
	Title   string
	Body    string
	Urgency Urgency
	Icon    string // freedesktop icon name
	Sound   Sound
}

// Sound is the kind of alert sound to play with a notification.
type Sound string

const (
	SoundFiring   Sound = "firing"
	SoundCritical Sound = "critical"
	SoundResolved Sound = "resolved"
)

// Sounds configures alert sounds. Files maps a sound to a user-provided
// audio file; sounds without one use the built-in file.
type Sounds struct {
	On    bool
	Files map[Sound]string
}

//go:embed sounds/*.ogg
var builtin embed.FS

// builtinFile is the embedded file each sound plays by default.
var builtinFile = map[Sound]string{
	SoundFiring:   "sounds/firing.ogg",
	SoundCritical: "sounds/firing.ogg",
	SoundResolved: "sounds/resolved.ogg",
}

type Notifier struct {
	sounds Sounds

	mu   sync.Mutex
	conn *dbus.Conn
	ids  map[string]uint32 // last notification ID per key
}

func New(s Sounds) *Notifier {
	return &Notifier{sounds: s, ids: map[string]uint32{}}
}

func (n *Notifier) SetSounds(s Sounds) {
	n.mu.Lock()
	n.sounds = s
	n.mu.Unlock()
}

const (
	dest  = "org.freedesktop.Notifications"
	path  = "/org/freedesktop/Notifications"
	iface = "org.freedesktop.Notifications"
)

// Send shows a notification. The D-Bus connection is opened lazily and
// reopened after errors, so healthd survives the desktop restarting.
func (n *Notifier) Send(m Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if err := n.connect(); err != nil {
		return err
	}

	// healthd plays its own sound, so ask the server not to add another.
	hints := map[string]dbus.Variant{
		"urgency":        dbus.MakeVariant(byte(m.Urgency)),
		"desktop-entry":  dbus.MakeVariant("healthd"),
		"suppress-sound": dbus.MakeVariant(true),
	}
	timeout := int32(10_000)
	if m.Urgency == Critical {
		timeout = 0 // stays until dismissed
	}

	var id uint32
	call := n.conn.Object(dest, path).Call(iface+".Notify", 0,
		"healthd", n.ids[m.Key], m.Icon, m.Title, m.Body, []string{}, hints, timeout)
	if err := call.Store(&id); err != nil {
		n.conn.Close()
		n.conn = nil
		return fmt.Errorf("send notification: %w", err)
	}
	n.ids[m.Key] = id

	if n.sounds.On && m.Sound != "" {
		go play(n.soundFile(m.Sound))
	}
	return nil
}

// soundFile returns the file to play for s: the configured one if set, else
// the built-in file.
func (n *Notifier) soundFile(s Sound) string {
	if file := n.sounds.Files[s]; file != "" {
		return file
	}
	file, err := extract(builtinFile[s])
	if err != nil {
		slog.Warn("extract built-in sound", "sound", s, "err", err)
		return ""
	}
	return file
}

var extractMu sync.Mutex

// extract copies an embedded sound to the cache directory, because players
// need a real file. The copy is rewritten when the embedded file changes.
func extract(name string) (string, error) {
	data, err := builtin.ReadFile(name)
	if err != nil {
		return "", err
	}
	file := filepath.Join(paths.CacheDir(), name)

	extractMu.Lock()
	defer extractMu.Unlock()
	if old, err := os.ReadFile(file); err == nil && bytes.Equal(old, data) {
		return file, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return file, os.Rename(tmp, file)
}

func (n *Notifier) connect() error {
	if n.conn != nil && n.conn.Connected() {
		return nil
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("connect to session bus: %w", err)
	}
	var caps []string
	if err := conn.Object(dest, path).Call(iface+".GetCapabilities", 0).Store(&caps); err != nil {
		conn.Close()
		return fmt.Errorf("no notification service on the session bus: %w", err)
	}
	n.conn = conn
	return nil
}

// players play an audio file and exit. Each is tried in order until one
// succeeds, so a player that can't decode the format (e.g. mp3 with an old
// libsndfile) falls through to the next.
var players = [][]string{
	{"pw-play"},
	{"paplay"},
	{"ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet"},
	{"canberra-gtk-play", "-f"},
}

func play(file string) {
	if file == "" {
		return
	}
	for _, p := range players {
		bin, err := exec.LookPath(p[0])
		if err != nil {
			continue
		}
		args := append(p[1:len(p):len(p)], file)
		err = exec.Command(bin, args...).Run()
		if err == nil {
			return
		}
		slog.Debug("play sound", "player", p[0], "file", file, "err", err)
	}
	slog.Warn("no player could play sound", "file", file)
}
