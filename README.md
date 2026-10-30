# healthd

A small health monitor for Linux machines. A background service watches a few key indicators (battery, temperatures, CPU load, memory, swap, disk usage, kernel errors) and sends a desktop notification when something goes wrong. A terminal dashboard shows the current state and recent history.

## Parts

- `healthd`: the background service. Detects hardware, samples indicators every 10s, runs alerts, sends notifications, keeps history, serves an API on a unix socket.
- `health`: the client. An interactive dashboard, plus one-shot commands for status, alert history and snoozes.

## Install

```sh
go install ./cmd/...           # installs health and healthd into ~/go/bin
healthd install-service        # run healthd in the background on login
```

For development, `go build -o bin/ ./cmd/...` builds into `./bin`.

## Run

```sh
healthd detect                  # what was detected and which thresholds apply
healthd                         # run in the foreground instead of as a service
```

On first run `healthd` writes a starter config to `~/.config/healthd/config.toml` with the detected hardware and every default as a comment. History goes to `~/.local/state/healthd/history.db`.

```sh
health                          # dashboard (q quits, ? shows keys)
health status                   # one-shot status table
health events                   # past alerts
health snooze cpu_load 1h       # or 30m, until-resolved
health unsnooze cpu_load
health reload                   # re-read config and re-detect hardware
```

## Kernel errors and the boot check

The kernel indicator watches the kernel log for errors: out of memory, I/O and filesystem errors, thermal problems, and PCIe link errors (e.g. a failing NVMe link logging `RxErr`). Since `healthd` starts at login, it also scans what was logged before: this boot so far and the previous boot, which catches a boot that froze. About 3 minutes after it starts it sends one summary notification per boot: what it found, or "all good".

Options live in `[kernel]` and `[kernel.boot_summary]` in the config; the starter config lists them with their defaults. See [Kernel errors](docs/design.md#kernel-errors) and [Boot check](docs/design.md#boot-check).

## Docs

- [Design](docs/design.md)
- [Portability: assumptions to remove](docs/portability.md)

## License

[MIT](LICENSE). The built-in alert sounds in `internal/notify/sounds/` are by Universfield on Pixabay and are under the Pixabay Content License instead; see [their README](internal/notify/sounds/README.md).
