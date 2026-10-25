package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/joaovictornsv/healthd/internal/api"
)

type server struct {
	http   *http.Server
	socket string
}

// listen opens the API socket. It refuses to start if another healthd
// already answers on it, and replaces a stale socket file otherwise.
func listen(socket string) (net.Listener, error) {
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		conn.Close()
		return nil, fmt.Errorf("healthd is already running (socket %s)", socket)
	}
	_ = os.Remove(socket)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (d *Daemon) serve(ln net.Listener) *server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", d.handleStatus)
	mux.HandleFunc("GET /v1/history", d.handleHistory)
	mux.HandleFunc("GET /v1/events", d.handleEvents)
	mux.HandleFunc("POST /v1/snooze", d.handleSnooze)
	mux.HandleFunc("DELETE /v1/snooze", d.handleUnsnooze)
	mux.HandleFunc("POST /v1/reload", d.handleReload)

	s := &server{http: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, socket: d.opts.SocketPath}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("api server stopped", "err", err)
		}
	}()
	return s
}

func (s *server) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	_ = os.Remove(s.socket)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, d.status())
}

var historyRanges = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

// maxPoints caps chart data; longer ranges are averaged into buckets.
const maxPoints = 360

func (d *Daemon) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "1h"
	}
	span, ok := historyRanges[rng]
	if !ok {
		http.Error(w, "range must be one of 15m, 1h, 24h, 7d", http.StatusBadRequest)
		return
	}
	if span <= ringWindow {
		if !d.has(id) {
			http.Error(w, errUnknownIndicator(id).Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, nonNil(d.ring(id, time.Now().Add(-span))))
		return
	}
	points, err := d.store.History(id, span, span/maxPoints)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, nonNil(points))
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	events, err := d.store.Events(r.URL.Query().Get("id"), min(limit, 1000))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []api.Event{}
	}
	writeJSON(w, events)
}

func (d *Daemon) handleSnooze(w http.ResponseWriter, r *http.Request) {
	var req api.SnoozeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := d.snooze(req.ID, req.Duration); err != nil {
		code := http.StatusBadRequest
		if isUnknown(err) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleUnsnooze(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	d.mu.Lock()
	found := false
	for _, ind := range d.inds {
		if ind.id == id {
			ind.snoozedUntil, ind.snoozeUntilResolved = time.Time{}, false
			found = true
		}
	}
	d.mu.Unlock()
	if !found {
		http.Error(w, errUnknownIndicator(id).Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleReload(w http.ResponseWriter, _ *http.Request) {
	if err := d.Reload(); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}

func nonNil(p []api.Point) []api.Point {
	if p == nil {
		return []api.Point{}
	}
	return p
}
