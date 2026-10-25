# Portability: assumptions to remove

The first design draft was written around a single laptop. This file lists the assumptions in that draft that won't hold on other Linux machines, and what the product should do instead.

## Assumptions

### 1. Hard-coded sensor names

The draft reads CPU temperature from hwmon `coretemp` and drive temperature from hwmon `nvme`.

- `coretemp` only exists on Intel CPUs. AMD uses `k10temp`, ARM boards often use `cpu_thermal` (thermal zone), and some laptops only expose `acpitz`.
- `nvme` only covers NVMe drives. SATA drives use `drivetemp` (when the module is loaded) or report nothing.

**Instead:** discover sensors at startup by scanning `/sys/class/hwmon/*/name` and `/sys/class/thermal/thermal_zone*`, with a known list of CPU sensor names in priority order. Allow overriding the choice in the config.

### 2. "SSD temperature" assumes one NVMe drive

**Instead:** call it "Disk temperature" and support zero, one or several drives. Show one line per drive, or one card per drive when there are few.

### 3. Disk usage is fixed to `/home`

Many machines have a single root filesystem and no separate `/home`.

**Instead:** default to `/` plus every other real mounted filesystem (skip `tmpfs`, `overlay`, `squashfs`, snap mounts, etc.). Let the config list the mounts to watch.

### 4. "Largest folders in `/home`" in the disk detail screen

Tied to one path, and walking a large home folder is slow.

**Instead:** drop it from v1, or run it only on demand for the selected mount, in the background, with a timeout.

### 5. A battery is assumed

Desktops and servers have none.

**Instead:** hide the battery card when `/sys/class/power_supply` has no battery. Support more than one battery (some laptops have two).

### 6. Swap is assumed to exist and be small

The 50% swap alert was tuned for a very small swap partition. Other machines may have no swap, zram, a swap file, or a large swap partition.

**Instead:** hide the swap card when there is no swap. Use a less aggressive default. Consider alerting on swap activity (pages swapped in/out per second from `/proc/vmstat`) as well as usage, since activity is the real sign of memory pressure.

### 7. Temperature thresholds tuned for specific hardware

The draft's CPU and drive thresholds came from one CPU and one SSD.

**Instead:** most hardware reports its own limits through hwmon (`temp*_max`, `temp*_crit`). Derive defaults from those (e.g. fire a few degrees below `max`, critical near `crit`) and fall back to fixed values only when the hardware doesn't report them. Show where each threshold came from (`hardware` / `default` / `config`).

### 8. Kernel error patterns biased to one setup

The draft's patterns mention ext4 and NVMe.

**Instead:** also cover btrfs, xfs and SATA (`ata` errors, link resets), and keep the pattern list in the config so users can extend it.

### 9. systemd is assumed

Both the service (`systemd --user`) and kernel logs (`journalctl`) depend on systemd.

- Distros without systemd (Alpine, Void, Gentoo with OpenRC, etc.) have neither.
- On some distros a normal user can only read kernel logs if they are in the `adm` or `systemd-journal` group.

**Instead:**
- Ship a `systemd --user` unit as the main path, but make `healthd` a plain foreground process so it can be started any other way (XDG autostart, OpenRC user service, a terminal).
- Read kernel messages from the journal when available, else from `/dev/kmsg`. If neither is readable, disable the kernel indicator and explain why in the TUI instead of failing silently.

### 10. A desktop session is assumed

`notify-send` and `paplay` don't work on headless machines, and `paplay` is PulseAudio-specific.

**Instead:**
- Send notifications directly over D-Bus (`org.freedesktop.Notifications`) with a `sound-name` hint. No external commands, and it works on PulseAudio and PipeWire setups.
- Put notification delivery behind a small interface so other channels (e.g. a webhook) can be added later.
- Decide whether headless machines are supported (see open questions).

### 11. The thresholds table describes one machine

**Instead:** document defaults and how they adapt to the hardware, not the numbers of one machine.

### 12. Installation isn't covered

**Instead:** provide:
- an install command (e.g. `go install`, plus release binaries later)
- `healthd install-service` to write and enable the systemd user unit
- a first run that detects hardware and writes a starter config with the detected sensors, drives, mounts, battery and swap, so users can see what was found and edit it

## Suggested design changes

- **Target platforms:** add a section stating what v1 supports. Proposal: Linux with a desktop session, x86_64 and arm64.
- **Hardware detection:** add a section. Detect sensors, drives, mounts, battery and swap at startup; hide cards for missing hardware; derive temperature defaults from hwmon limits.
- **Indicator names:** "SSD temp" → "Disk temp", "Disk /home" → "Disk usage", both covering several devices or mounts.
- **Notifications:** D-Bus instead of `notify-send` + `paplay`, behind a notifier interface.
- **Installation:** add a section covering install, service setup and first-run config.
- **Keep the design doc generic:** no machine-specific paths, sizes, tools or desktop environments.

## Open questions

- Support headless machines in v1? If yes, alerts need a non-desktop channel (webhook, email, or just the TUI and logs).
- Minimum supported kernel / distro baseline?
- Should drives and mounts that appear later (USB disks) be picked up live, or only at startup?
