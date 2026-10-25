// Package notify sends desktop notifications through the freedesktop
// notification service on the session D-Bus.
package notify

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/godbus/dbus/v5"
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
	Sound   string // freedesktop sound name
}

type Notifier struct {
	sound bool

	mu         sync.Mutex
	conn       *dbus.Conn
	serverPlay bool              // server plays sound-name hints itself
	ids        map[string]uint32 // last notification ID per key
}

func New(sound bool) *Notifier {
	return &Notifier{sound: sound, ids: map[string]uint32{}}
}

func (n *Notifier) SetSound(on bool) {
	n.mu.Lock()
	n.sound = on
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

	hints := map[string]dbus.Variant{
		"urgency":       dbus.MakeVariant(byte(m.Urgency)),
		"desktop-entry": dbus.MakeVariant("healthd"),
	}
	if n.sound && m.Sound != "" {
		hints["sound-name"] = dbus.MakeVariant(m.Sound)
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

	if n.sound && m.Sound != "" && !n.serverPlay {
		go playSound(m.Sound)
	}
	return nil
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
	n.serverPlay = false
	for _, c := range caps {
		if c == "sound" {
			n.serverPlay = true
		}
	}
	n.conn = conn
	return nil
}

// playSound plays a freedesktop sound with whichever player is installed.
func playSound(name string) {
	if bin, err := exec.LookPath("canberra-gtk-play"); err == nil {
		run(bin, "-i", name)
		return
	}
	file := filepath.Join("/usr/share/sounds/freedesktop/stereo", name+".oga")
	if _, err := os.Stat(file); err != nil {
		return
	}
	for _, player := range []string{"pw-play", "paplay"} {
		if bin, err := exec.LookPath(player); err == nil {
			run(bin, file)
			return
		}
	}
}

func run(bin string, args ...string) {
	if err := exec.Command(bin, args...).Run(); err != nil {
		slog.Debug("play sound", "player", bin, "err", err)
	}
}
