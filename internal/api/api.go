// Package api defines the JSON types served by healthd on its unix socket
// and a small client for them. It has no heavy dependencies so the TUI
// binary stays small.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Status struct {
	Host           string      `json:"host"`
	UptimeSeconds  int64       `json:"uptime_seconds"`
	StartedAt      time.Time   `json:"started_at"`
	SampledAt      time.Time   `json:"sampled_at"`
	SampleInterval string      `json:"sample_interval"`
	ConfigPath     string      `json:"config_path"`
	Warnings       []string    `json:"warnings,omitempty"`
	Indicators     []Indicator `json:"indicators"`
}

type Indicator struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Device string   `json:"device,omitempty"`
	Unit   string   `json:"unit"`
	Value  *float64 `json:"value"`
	Status string   `json:"status,omitempty"`
	Error  string   `json:"error,omitempty"`

	State    string    `json:"state"`
	Critical bool      `json:"critical"`
	Since    time.Time `json:"since"`
	Active   bool      `json:"active"`

	Thresholds          Thresholds `json:"thresholds"`
	SnoozedUntil        *time.Time `json:"snoozed_until,omitempty"`
	SnoozeUntilResolved bool       `json:"snooze_until_resolved,omitempty"`

	// Spark holds the last 15 minutes of values, oldest first.
	Spark  []float64      `json:"spark"`
	Detail map[string]any `json:"detail,omitempty"`
}

type Thresholds struct {
	Direction  string   `json:"direction"` // "above" or "below"
	FireAt     float64  `json:"fire_at"`
	For        string   `json:"for"`
	ResolveAt  float64  `json:"resolve_at"`
	CriticalAt *float64 `json:"critical_at,omitempty"`
	Source     string   `json:"source"` // hardware, default, config
}

type Point struct {
	T int64   `json:"t"` // unix seconds
	V float64 `json:"v"`
}

type Event struct {
	Time      time.Time `json:"time"`
	Indicator string    `json:"indicator"`
	Kind      string    `json:"kind"` // fired, escalated, resolved
	Critical  bool      `json:"critical"`
	Value     float64   `json:"value"`
	Message   string    `json:"message"`
}

type SnoozeRequest struct {
	ID string `json:"id"`
	// Duration is a Go duration ("30m", "1h") or "until_resolved".
	Duration string `json:"duration"`
}

const UntilResolved = "until_resolved"

type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}}
}

func (c *Client) Status() (*Status, error) {
	var s Status
	return &s, c.do(http.MethodGet, "/v1/status", nil, &s)
}

func (c *Client) History(id, rng string) ([]Point, error) {
	var p []Point
	q := url.Values{"id": {id}, "range": {rng}}
	return p, c.do(http.MethodGet, "/v1/history?"+q.Encode(), nil, &p)
}

func (c *Client) Events(id string, limit int) ([]Event, error) {
	var e []Event
	q := url.Values{"id": {id}, "limit": {strconv.Itoa(limit)}}
	return e, c.do(http.MethodGet, "/v1/events?"+q.Encode(), nil, &e)
}

func (c *Client) Snooze(id, duration string) error {
	return c.do(http.MethodPost, "/v1/snooze", SnoozeRequest{ID: id, Duration: duration}, nil)
}

func (c *Client) Unsnooze(id string) error {
	return c.do(http.MethodDelete, "/v1/snooze?"+url.Values{"id": {id}}.Encode(), nil, nil)
}

func (c *Client) Reload() error {
	return c.do(http.MethodPost, "/v1/reload", nil, nil)
}

func (c *Client) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://healthd"+path, r)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach healthd (is it running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("healthd: %s", bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
