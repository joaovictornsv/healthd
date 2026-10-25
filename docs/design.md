# healthd — Design

A small, always-on health monitor for Linux machines. It watches a few key indicators, sends a desktop notification when something goes wrong, and includes a terminal dashboard showing current state and recent history.

## Goals

- **Simple:** one binary per part, one config file, no external services
- **Few indicators:** only the ones that tell you a machine is in trouble
- **Works anywhere:** adapts to the hardware it finds; nothing hard-coded to one machine
- **Good UI:** see the whole machine at a glance (TUI)
- **Independent alerts:** alerts keep working when the UI is closed

## Target platforms (v1)

- Linux with a desktop session (notifications go through the freedesktop D-Bus notification service)
- x86_64 and arm64
- systemd is the main path (service + kernel journal), but not required: `healthd` is a plain foreground process and falls back to `/dev/kmsg` for kernel messages

Headless machines are not a v1 target (see open questions).

## Architecture

Split into a watcher (always running) and a viewer (opened when wanted).

```
healthd (background service)
  ├─ detects hardware at startup
  ├─ samples every 10s
  ├─ one alert engine: OK → PENDING → FIRING → RESOLVED
  ├─ notifications over D-Bus (with sound)
  ├─ history: in-memory ring buffer (live view) + SQLite (persisted)
  └─ HTTP/JSON API on a unix socket

health (client)
  └─ connects to the socket → live values, sparklines, alerts, snooze
```

- **healthd** runs as a `systemd --user` service so it has the session environment (D-Bus address). It can also run from a terminal or any other user-service manager.
- **health** is a client only. Closing it does not affect monitoring.
- Socket: `$XDG_RUNTIME_DIR/healthd.sock` (fallback `/tmp/healthd-<uid>.sock`), mode `0600`.

## Stack

- **Go:** single static binary (no cgo), low idle cost
- Metrics read directly from `/proc` and `/sys` (no metrics library needed on Linux)
- **godbus/dbus:** notifications
- **modernc.org/sqlite:** history (pure Go, keeps the binary static)
- **BurntSushi/toml:** config
- **Bubble Tea + Lipgloss:** TUI

## Indicators

| Indicator | ID | Source | Shown when |
|---|---|---|---|
| Battery | `battery` | `/sys/class/power_supply` (type `Battery`, not peripheral) | a system battery exists |
| CPU temperature | `cpu_temp` | hwmon / thermal zones (see detection) | a CPU sensor is found |
| Disk temperature | `disk_temp:<drive>` | hwmon `nvme`, `drivetemp` | per drive that reports temperature |
| CPU load | `cpu_load` | `/proc/loadavg` (5-min average as % of CPUs) | always |
| Memory | `memory` | `/proc/meminfo` (100 − MemAvailable %) | always |
| Swap | `swap` | `/proc/meminfo` | swap is configured |
| Disk usage | `disk_usage:<mount>` | `statfs` | per real mounted filesystem |
| Kernel errors | `kernel` | journal (`journalctl -k`), else `/dev/kmsg` | always (shows why if unreadable) |

Several batteries are combined into one value.

## Hardware detection

Runs at startup and on config reload.

- **CPU temperature:** first match wins.
  1. hwmon by name: `coretemp` (label `Package id 0`), `k10temp` (`Tctl`, then `Tdie`), `zenpower` (`Tdie`), `cpu_thermal` / `cpu-thermal` / `soc_thermal`
  2. thermal zones by type: `x86_pkg_temp`, `cpu-thermal`, `cpu_thermal`, `soc_thermal`, then `acpitz`
  3. config can force a hwmon name and label
- **Drives:** every hwmon named `nvme` or `drivetemp`, labelled with the device name (`nvme0`, `sda`).
- **Mounts:** real filesystems from `/proc/self/mounts` (`ext2/3/4`, `xfs`, `btrfs`, `f2fs`, `zfs`, `bcachefs`, …). Skips pseudo, network, FUSE and FAT filesystems, and paths under `/snap`, `/run`, `/var/lib/docker`. Deduplicated by device (btrfs subvolumes). Config can list mounts explicitly.
- **Battery:** power supplies with `type=Battery` and `scope` other than `Device` (skips mouse/keyboard batteries).
- **Swap:** `SwapTotal > 0`.

Hardware that isn't found is simply not shown.

## Alert model

Same engine for every indicator:

- **OK → PENDING** when the value crosses `fire_at`
- **PENDING → FIRING** if it stays there for `for`
- **Critical** skips the pending period and escalates immediately
- **Repeat** a reminder every `repeat_interval` while firing
- **RESOLVED** notification when the value goes back past `resolve_at` (hysteresis avoids flapping)

Direction depends on the indicator: most fire when the value goes **above** the threshold; battery fires when it goes **below**. Battery alerts only apply while discharging; plugging in clears them silently.

## Default thresholds

Temperature defaults come from the hardware's own limits when available (hwmon `temp*_crit` / `temp*_max`, thermal-zone critical trip point). They are early warnings ("slow down"), not danger limits.

| Indicator | Fire at | For | Resolve at | Critical at |
|---|---|---|---|---|
| Battery (discharging) | ≤ 20% | — | > 25% | ≤ 10% |
| CPU temperature | crit − 20 (fallback 80°C) | 5 min | fire − 10 | crit − 8 (fallback 92°C) |
| Disk temperature | max − 20 (fallback 60°C) | 3 min | fire − 7 | max − 5 (fallback 75°C) |
| CPU load | 80% | 5 min | 60% | — |
| Memory | 90% | 2 min | 80% | 97% |
| Swap | 80% | 5 min | 60% | — |
| Disk usage | 85% | 5 min | 80% | 95% |
| Kernel errors | ≥ 1 match in 5 min | — | 0 | always critical |

Repeat interval: 10 minutes. The status API reports where each threshold came from: `hardware`, `default` or `config`.

## Notifications

- Sent directly over D-Bus (`org.freedesktop.Notifications`), no external commands.
- Urgency `normal` for firing, `critical` for critical, `low` for resolved.
- Each indicator reuses its notification ID, so a repeat or resolve replaces the previous bubble instead of stacking.
- Sound: one sound for firing, one for critical, one for resolved. Built-in sounds (Ogg Vorbis) are embedded in the binary, so nothing needs installing; they are copied to `$XDG_CACHE_HOME/healthd/sounds/` the first time they play, since players need a file. Each can be replaced in the config by any audio file. healthd plays the file itself (`pw-play`, then `paplay`, `ffplay`, `canberra-gtk-play`, moving on if one can't decode it) and sends the `suppress-sound` hint so the notification server doesn't add its own.
- CPU load, memory and swap notifications include the top processes.

## Kernel errors

Only error-level kernel messages (priority ≤ 3) that match a pattern count. Default patterns (case-insensitive, extendable in config):

- memory: `out of memory`, `oom-kill`
- storage: `i/o error`, `blk_update_request`, `nvme.*(timeout|reset|error)`, `ata\d+.*(failed|error|exception|hard resetting)`
- filesystems: `ext4-fs error`, `btrfs.*(error|corrupt)`, `xfs.*(error|corrupt|shutdown)`
- hardware: `thermal`, `throttl`, `mce:`, `hardware error`

Source: a long-running `journalctl -k -p err -f -o json`, else `/dev/kmsg`. If neither is readable, the indicator shows the reason (e.g. user not in the `adm`/`systemd-journal` group).

## Config

Single file at `$XDG_CONFIG_HOME/healthd/config.toml` (default `~/.config/healthd/config.toml`).

On first run `healthd` writes a starter config listing the detected hardware and the effective defaults as comments. Anything left commented out uses the default. Reload with `SIGHUP` or `r` in the TUI.

```toml
sample_interval = "10s"
repeat_interval = "10m"

[notify]
enabled = true
sound = true
# firing_sound = "sounds/firing.mp3"   # relative to this file's folder; "~" works too
# critical_sound = "sounds/critical.mp3"
# resolved_sound = "sounds/resolved.mp3"

[cpu_temp]
# sensor = "coretemp"     # hwmon name; detected automatically
# label = "Package id 0"
fire_at = 75
for = "5m"

[battery]
fire_at = 30
critical_at = 15

[disk_usage]
# mounts = ["/", "/data"] # default: all detected
fire_at = 90

[kernel]
# extra_patterns = ["amdgpu.*timeout"]
```

Every indicator section accepts `enabled`, `fire_at`, `for`, `resolve_at`, `critical_at`.

## History

- **In memory:** the last hour at full resolution, for the live view. Loaded from SQLite on startup so sparklines survive restarts.
- **SQLite** at `$XDG_STATE_HOME/healthd/history.db` (default `~/.local/state/healthd/history.db`):
  - raw samples: 24 hours
  - 1-minute averages: 30 days
  - alert events (fired / escalated / resolved): 90 days
- Why persist: restarts and reboots are exactly when you want to look back (overheating overnight, an OOM before a freeze). Size stays in the low MBs.

## API (unix socket, HTTP/JSON)

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/status` | host info, all indicators with value, state, thresholds, sparkline, details |
| GET | `/v1/history?id=&range=` | chart data: `15m`, `1h` (memory), `24h`, `7d` (SQLite) |
| GET | `/v1/events?id=&limit=` | past alert events |
| POST | `/v1/snooze` | `{"id": "cpu_load", "duration": "1h"}` or `"until_resolved"` |
| DELETE | `/v1/snooze?id=` | cancel snooze |
| POST | `/v1/reload` | reload config and re-detect hardware |

## TUI

A dashboard first, not a menu. The point of the TUI is a glance: open it and know within a second whether something is wrong. All indicators fit on one screen. Depth (long charts, top processes, past alerts) lives in a detail screen opened from each card.

Three screens:

### 1. Dashboard (home)

```
 health · myhost · up 3d 4h                     ● 1 firing  ◐ 1 pending   updated 4s ago
╭─ Battery ─────────────────╮╭─ CPU temp ────────────────╮╭─ Disk temp ───────────────╮
│ 62%           discharging ││ 84°C              FIRING  ││ nvme0  41°C           OK  │
│ ████████████████░░░░░░░░░ ││ ▂▃▃▅▆▇▇▇▇▇▇▇▆▇▇▇▇▇▇▇▇▇▇▇▇ ││ sda    33°C           OK  │
│ 2h 14m left               ││ for 7m · fire 80 crit 92  ││                           │
╰───────────────────────────╯╰───────────────────────────╯╰───────────────────────────╯
╭─ CPU load ────────────────╮╭─ Memory ──────────────────╮╭─ Swap ────────────────────╮
│ 83%              PENDING  ││ 61%  19.5 / 32 GB     OK  ││ 3%  120 MB / 4 GB     OK  │
│ ▃▄▅▆▇▇▇▇▇▇▇▇▇▇▆▇▇▇▇▇▇▇▇▇▇ ││ ▅▅▅▅▅▅▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆ ││ ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁ │
│ 3/5m · make (612%) node   ││ fire 90                   ││ fire 80                   │
╰───────────────────────────╯╰───────────────────────────╯╰───────────────────────────╯
╭─ Disk usage ──────────────╮╭─ Kernel ─────────────────────────────────────────────────╮
│ /      72% ████████░░  OK ││ no errors in the last 24h                            OK  │
│ /data  41% ████░░░░░░  OK ││                                                          │
╰───────────────────────────╯╰──────────────────────────────────────────────────────────╯
 ←↑↓→ move   enter details   a alerts   s snooze   ? help   q quit
```

- **Card:** current value, state, a 15-minute sparkline, and one line of context (thresholds, time left, top process)
- **Multi-device cards:** disk temperature and disk usage show one line per drive or mount; the card's colour follows the worst one
- **Colour by state:** card border and state label are green (OK), yellow (PENDING), red (FIRING), bright red (CRITICAL). A problem should be visible before reading any number.
- **Header:** summary of active alerts (`● 1 firing ◐ 1 pending`) and time since the last sample
- **Bar vs sparkline:** slow-moving values (battery, disk usage) get a bar; moving values (temperatures, load, memory, swap) get a sparkline
- **Missing hardware:** cards for hardware that isn't present are not shown; the grid reflows
- **Responsive:** 3 columns on wide terminals, 2 on medium, and below ~60 columns a compact one-line-per-indicator list (`CPU temp  84°C  FIRING  ▂▃▅▇▇▇`)

### 2. Detail screen (Enter on a card)

```
 CPU temp                                                        FIRING for 7m · 84°C
 [15m]  1h   24h   7d                                         min 52 · avg 74 · max 88
 92 ┤┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈ critical
 80 ┤┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈⣠⣴⣶⣾⣿⣷⣶⣾⣿⣿⣷⣶⣾⣿⣿⣿⣿⣷⣶⣾⣿⣿⣿⣷⣶⣶⣾⣿⣿⣿┈┈┈┈┈┈┈┈┈┈┈┈┈┈ fire
 70 ┤                ⢀⣠⣴⡾⠋
 60 ┤      ⢀⣀⣠⣤⠴⠖⠋⠉
 50 ┤⣀⣀⡤⠔⠋
    └────────────────────────────────────────────────────────────────────────────
 Top processes (CPU)                     Recent alerts
   make            612%                    today 14:02  FIRING     81°C
   node            145%                    today 11:40  RESOLVED   68°C
   firefox          38%                    yesterday    CRITICAL   93°C
 esc back   1-4 range   ←→ prev/next indicator   s snooze
```

- **Chart:** braille line chart with dashed lines at the fire and critical thresholds
- **Ranges:** 15m and 1h from memory; 24h and 7d from SQLite
- **Stats:** min / avg / max for the selected range
- **Context panel per indicator:**
  - CPU load, CPU temp: top processes by CPU
  - Memory, swap: top processes by memory
  - Battery: charge/discharge rate, cycle count
  - Disk usage: per-mount used / free
  - Kernel: full error lines
- **Thresholds** with their source (`hardware` / `default` / `config`)
- **Recent alerts** for this indicator
- `←` / `→` jump to the previous/next indicator without going back to the dashboard

### 3. Alerts screen (`a`)

```
 Alerts                                                                    [active] history
 ● FIRING    CPU temp   84°C   since 14:02 (7m)
 ◐ PENDING   CPU load   83%    since 14:06 (3m of 5m)
 ─────────────────────────────────────────────────────────
 today 11:40   RESOLVED   CPU temp   68°C   lasted 22m
 today 09:12   FIRING     Battery    18%    discharging
 yesterday     CRITICAL   Disk temp  nvme0 66°C   lasted 4m
```

Active alerts on top, past alert events from SQLite below. Answers "what happened while I wasn't looking?".

### Snooze

`s` on a card or detail screen silences that indicator for 30m, 1h, or until it resolves. Useful before a known heavy job. The TUI sends the snooze to `healthd` over the socket, so the dashboard still shows the real state but no notifications are sent. Snoozed cards show a `zz` marker. Without this, the temptation is to raise thresholds instead.

### Keys

| Key | Action |
|---|---|
| `←↑↓→` / `hjkl` | move between cards |
| `enter` | open detail |
| `esc` | back |
| `1`–`4` | chart range (detail screen) |
| `a` | alerts screen |
| `s` | snooze |
| `r` | reload config |
| `?` | help |
| `q` | quit |

### Not in v1

- Mouse support
- Editing thresholds inside the TUI (edit the config file, press `r`)
- Themes
- Screens beyond these three

## CLI

- `healthd`: run the service in the foreground
- `healthd detect`: print detected hardware and effective thresholds, then exit
- `healthd install-service`: write and enable the `systemd --user` unit
- `health`: open the TUI
- `health status`: one-shot status table (scripts, quick checks)
- `health snooze <id> <30m|1h|until-resolved>` / `health unsnooze <id>`

## Installation

- `go install github.com/joaovictornsv/healthd/cmd/...@latest` (release binaries later)
- `healthd install-service` to run it in the background on login
- First run writes the starter config

## Out of scope (for now)

- Web UI
- Prometheus / Grafana
- Monitoring multiple machines
- Plugin system

Possible later add-ons: a tray icon that changes colour when an alert is active; swap activity (pages in/out per second) as a memory-pressure signal.

## Open questions

- Support headless machines? If yes, alerts need a non-desktop channel (webhook, email, or just the TUI and logs).
- Minimum supported kernel / distro baseline?
- Should drives and mounts that appear later (USB disks) be picked up live, or only on reload?
