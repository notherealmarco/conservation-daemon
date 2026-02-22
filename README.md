# Conservation Daemon

A simple smart charge controller for Lenovo Yoga/IdeaPad laptops on Linux that handles battery conservation mode to extend battery lifespan, which allows to configure the time to reach a specific battery level (e.g., "charge to 95% by 9:00 AM") using a non-root CLI.

## Requirements

- Linux system with UPower daemon
- Lenovo laptop with `ideapad_laptop` kernel module loaded
- Conservation mode support in `/sys/bus/platform/drivers/ideapad_acpi/*/conservation_mode`
- For the tray icon: `gtk3`, `libayatana-appindicator`, and `zenity`

## Installation

### Pre-built Releases

Download the latest release from: https://git.marcorealacci.me/marcorealacci/conservation-daemon/releases

### Arch Linux (AUR)

```bash
# Using your favorite AUR helper
paru -S conservation-daemon-bin
# or
yay -S conservation-daemon-bin
```

> [!IMPORTANT]  
> Make sure a group called `conservationd` exists and add your user to it to allow non-root CLI access.

### Build from Source

**Prerequisites:**
- Go 1.25 or later

**Build:**
```bash
git clone <repository-url>
cd conservationDaemon
go build -o conservationd ./cmd/daemon   # Builds daemon executable
go build -o conservationctl ./cmd/cli    # Builds cli executable
go build -o conservation-tray ./cmd/tray # Builds tray executable
```

## Usage

### Basic Commands

**Start the daemon (as root):**
```bash
sudo systemctl enable --now conservationd
```

**Check current status:**
```bash
conservationctl
# Output: pct=85.0 state=charging cons=0 max=80.0 time=now
```

**Set immediate charging target:**
```bash
conservationctl -set -max 90
# Charges to 90%, then enables conservation mode immediately afterwards
```

**Schedule charging for specific time:**
```bash
conservationctl -set -max 95 -time 9:00
# Will charge to 95% by 9:00 AM tomorrow
# if the specified time is in the past, it assumes the next day
```

### Run the tray icon
```bash
# One-time: enable the user service
systemctl --user enable --now conservation-tray

# Or run manually:
conservation-tray
```

### Auto Mode

Auto mode enables/disables conservation based on external display connection:
- **External display connected** → conservation ON (you're at a desk)
- **No external display** → conservation OFF (you're mobile, let it charge)

```bash
# Enable auto mode
conservationctl -set -auto

# Disable auto mode
conservationctl -set
```

Auto mode state persists across daemon restarts via the state file.

### Daemon Options

```bash
./conservationd [options]
  -max float
        target maximum percentage (default 80)
  -conservation-threshold float
        battery percentage at which conservation mode activates (default 80)
  -interval duration
        poll interval (default 45s)
  -dry-run
        do not write sysfs, only log actions
  -once
        perform a single control step and exit
  -sysfs string
        explicit conservation_mode path (auto-discovered if empty)
  -sock string
        UNIX control socket path (default "/run/conservationd/conservationd.sock")
  -sock-group string
        group name to own the socket (default "conservationd")
  -auto
        enable conservation based on external display connection
  -state string
        path to persist runtime state (default "/var/lib/conservationd/state.json")
  -version
        print version and exit
```

### CLI Options

```bash
./conservationctl [options]
  -set
        set new thresholds and/or time
  -max float
        target maximum percentage (default 80)
  -time string
        target time in HH:MM format (default "now")
  -status
        show detailed status (same as default behavior)
  -sock string
        control socket path (default "/run/conservationd/conservationd.sock")
  -auto
        enable auto mode (display sensing)
  -version
        print version and exit
```

## Troubleshooting

**Conservation mode file not found:**
```bash
# Check if ideapad_laptop module is loaded
lsmod | grep ideapad
# Load if missing
sudo modprobe ideapad_laptop

# Find conservation mode file
find /sys -name "conservation_mode" 2>/dev/null
```

**Permission denied on socket:**
```bash
# Check if user is in conservationd group
groups
# Add user to group if missing
sudo usermod -a -G conservationd $USER
# Log out and back in
```

**Daemon not responding:**
```bash
# Check daemon status
sudo systemctl status conservationd
# Check logs
journalctl -u conservationd -f
```

## License

MIT License - see LICENSE file for details.

## Contributing

Issues and pull requests welcome!