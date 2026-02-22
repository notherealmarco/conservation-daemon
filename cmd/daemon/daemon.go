// SPDX-License-Identifier: MIT
// Requires: UPower daemon, ideapad_laptop kernel module.
// Caveat: Conservation mode is binary and typically targets ~80% when enabled.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// Version metadata injected at build time via -ldflags
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type BatteryState uint32

const (
	BatteryStateUnknown   BatteryState = 0
	BatteryStateCharging  BatteryState = 1
	BatteryStateDischarge BatteryState = 2
	BatteryStateEmpty     BatteryState = 3
	BatteryStateFull      BatteryState = 4
	BatteryStatePending   BatteryState = 5
)

type Config struct {
	MaxPercent            float64
	ConservationThreshold float64
	PollInterval          time.Duration
	DryRun                bool
	Once                  bool
	Auto                  bool
	SysfsPath             string

	// Control socket
	SockPath  string
	SockGroup string

	// Time-based charging
	TargetTime   *time.Time
	LevelReached bool // true when target percentage has been reached
}

type SharedState struct {
	mu      sync.Mutex
	cfg     Config
	pct     float64
	bstate  BatteryState
	cons    int
	lastErr string
}

type Req struct {
	Cmd  string  `json:"cmd"`
	Max  float64 `json:"max,omitempty"`
	Time string  `json:"time,omitempty"` // Time in HH:MM format or "now"
	Auto *bool   `json:"auto,omitempty"`
}

type Resp struct {
	Ok    bool    `json:"ok"`
	Msg   string  `json:"msg,omitempty"`
	Max   float64 `json:"max,omitempty"`
	Pct   float64 `json:"pct,omitempty"`
	State string  `json:"state,omitempty"`
	Cons  int     `json:"cons,omitempty"`
	Time  string  `json:"time,omitempty"` // Target time or "now"
	Auto  bool    `json:"auto,omitempty"`
}

func main() {
	cfg := parseFlags()

	if cfg.MaxPercent < cfg.ConservationThreshold || cfg.MaxPercent > 100 {
		exitErr(fmt.Errorf("max must be in [%.1f,100], got %.1f", cfg.ConservationThreshold, cfg.MaxPercent))
	}
	if cfg.ConservationThreshold < 50 || cfg.ConservationThreshold > 100 {
		exitErr(fmt.Errorf("conservation-threshold must be in [50,100], got %.1f", cfg.ConservationThreshold))
	}

	conspath := cfg.SysfsPath
	if conspath == "" {
		var err error
		conspath, err = findConservationNode()
		if err != nil {
			exitErr(err)
		}
	}

	ctx := context.Background()
	conn, err := dbus.SystemBus()
	if err != nil {
		exitErr(fmt.Errorf("connect system bus: %w", err))
	}
	defer conn.Close()

	batPath, err := findDisplayBattery(ctx, conn)
	if err != nil {
		exitErr(err)
	}

	logf("Using battery path: %s", batPath)
	logf("Using sysfs path: %s", conspath)

	// Shared state for control-plane
	st := &SharedState{cfg: cfg}

	// Start control socket (unless Once mode)
	var ln net.Listener
	if !cfg.Once && cfg.SockPath != "" {
		ln, err = setupSocket(cfg.SockPath, cfg.SockGroup)
		if err != nil {
			exitErr(err)
		}
		defer ln.Close()
		go acceptLoop(ln, st)
	}

	if cfg.Once {
		runOnce(ctx, conn, batPath, conspath, st)
		return
	}

	t := time.NewTicker(cfg.PollInterval)
	defer t.Stop()

	for {
		runOnce(ctx, conn, batPath, conspath, st)
		select {
		case <-t.C:
			continue
		}
	}
}

func parseFlags() Config {
	showVersion := flag.Bool("version", false, "print version and exit")
	max := flag.Float64("max", 80, "target maximum percentage to start capping (80..100)")
	conservationThreshold := flag.Float64("conservation-threshold", 80, "battery percentage at which conservation mode activates (default varies by laptop model)")
	interval := flag.Duration("interval", 45*time.Second, "poll interval")
	dry := flag.Bool("dry-run", false, "do not write sysfs, only log actions")
	once := flag.Bool("once", false, "perform a single control step and exit")
	auto := flag.Bool("auto", false, "enable/disable conservation mode based on external monitor connection status")
	sysfs := flag.String("sysfs", "", "explicit conservation_mode path; auto-discover if empty")
	sock := flag.String("sock", "/run/conservationd/conservationd.sock", "UNIX control socket path ('' to disable)")
	sockGroup := flag.String("sock-group", "conservationd", "group name to own the socket (0660)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("conservationd %s (commit %s, built %s) %s/%s\n", version, commit, date, runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}
	return Config{
		MaxPercent:            *max,
		ConservationThreshold: *conservationThreshold,
		PollInterval:          *interval,
		DryRun:                *dry,
		Once:                  *once,
		Auto:                  *auto,
		SysfsPath:             *sysfs,
		SockPath:              *sock,
		SockGroup:             *sockGroup,
	}
}

func runOnce(ctx context.Context, conn *dbus.Conn, batPath dbus.ObjectPath, conspath string, st *SharedState) {
	// Snapshot thresholds under lock
	st.mu.Lock()
	cfg := st.cfg
	st.mu.Unlock()

	pct, state, err := readUPower(ctx, conn, batPath)
	if err != nil {
		st.mu.Lock()
		st.lastErr = err.Error()
		st.mu.Unlock()
		logf("read upower error: %v", err)
		return
	}
	cur, err := readConservation(conspath)
	if err != nil {
		st.mu.Lock()
		st.lastErr = err.Error()
		st.mu.Unlock()
		logf("read cons error: %v", err)
		return
	}

	action := "none"
	want := cur

	// Determine base desired state from auto mode
	extConn := false
	if cfg.Auto {
		var err error
		extConn, err = isExternalDisplayConnected()
		if err != nil {
			logf("check external display error: %v", err)
		}
	}

	// If max percentage is at or below conservation threshold, enable conservation
	// BUT if auto mode is on, defer to the display connection status
	if cfg.MaxPercent <= cfg.ConservationThreshold {
		if cfg.Auto && !extConn {
			want = 0
			action = "disable_conservation_display_disconnected"
		} else {
			want = 1
			action = "enable_conservation_threshold_mode"
		}
	} else {
		// Check if we've reached the target level
		if !cfg.LevelReached && pct >= cfg.MaxPercent {
			st.mu.Lock()
			st.cfg.LevelReached = true
			st.mu.Unlock()
			cfg.LevelReached = true
		}

		if cfg.TargetTime != nil {
			// Time-based charging logic
			now := time.Now()
			target := *cfg.TargetTime

			// Calculate when to start charging (assuming 1 minute per 1%)
			chargingTimeNeeded := time.Duration(cfg.MaxPercent-pct) * time.Minute
			startTime := target.Add(-chargingTimeNeeded)

			logf("schedule mode: target=%.1f%% at %s, current=%.1f%%, start_time=%s, level_reached=%t",
				cfg.MaxPercent, target.Format("2006-01-02 15:04"), pct, startTime.Format("15:04"), cfg.LevelReached)

			switch {
			case cfg.LevelReached:
				// Level reached - keep conservation enabled and clear schedule if target time passed
				want = 1
				action = "enable_conservation_level_reached"
				if now.After(target) {
					st.mu.Lock()
					st.cfg.TargetTime = nil
					st.mu.Unlock()
					action = "enable_conservation_schedule_completed"
				}
			case now.After(target):
				// Target time passed but level not reached - clear schedule
				st.mu.Lock()
				st.cfg.TargetTime = nil
				st.mu.Unlock()
				logf("target time passed without reaching level, clearing schedule")
				// Apply immediate logic
				if cfg.Auto {
					if extConn {
						want = 1
						action = "enable_conservation_display_connected"
					} else {
						want = 0
						action = "disable_conservation_display_disconnected"
					}
				} else {
					if pct >= cfg.MaxPercent {
						want = 1
						action = "enable_conservation_immediate"
					} else {
						want = 0
						action = "disable_conservation_immediate"
					}
				}
			case now.After(startTime):
				// Time to start charging
				want = 0
				action = "disable_conservation_scheduled_charging"
			case pct >= cfg.MaxPercent:
				// Reached target percentage - enable conservation and mark level reached
				want = 1
				action = "enable_conservation_target_percentage_reached"
				st.mu.Lock()
				st.cfg.LevelReached = true
				st.mu.Unlock()
			default:
				// Not time to charge yet
				if cfg.Auto && !extConn {
					// Auto mode: monitor disconnected, force conservation off
					want = 0
					action = "disable_conservation_display_disconnected"
				} else {
					// Either Auto mode with display connected, or normal schedule waiting
					want = 1
					action = "enable_conservation_waiting_for_schedule"
				}
			}
		} else {
			// Immediate charging logic
			if cfg.LevelReached {
				// Level reached - keep conservation enabled
				want = 1
				action = "enable_conservation_level_reached"
			} else {
				if cfg.Auto {
					if extConn {
						want = 1
						action = "enable_conservation_display_connected"
					} else {
						want = 0
						action = "disable_conservation_display_disconnected"
					}
				} else {
					// Level not reached yet - disable conservation to charge
					want = 0
					action = "disable_conservation_charging_to_target"
				}
			}
		}
	}

	logf("pct=%.1f state=%s conservation=%d action=%s target=%.1f level_reached=%t",
		pct, stateString(state), cur, action, cfg.MaxPercent, cfg.LevelReached)

	if want != cur {
		if cfg.DryRun {
			logf("[dry-run] would write %d to %s", want, conspath)
		} else {
			if err := writeConservation(conspath, want); err != nil {
				logf("write cons error: %v", err)
			} else {
				logf("conservation set to %d", want)
			}
		}
	}

	// Publish new measurements
	st.mu.Lock()
	st.pct = pct
	st.bstate = state
	st.cons = want
	st.mu.Unlock()
}

func setupSocket(sockPath, group string) (net.Listener, error) {
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	_ = os.RemoveAll(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	// chgrp directory and socket so group members can connect
	if g, err := user.LookupGroup(group); err == nil {
		if gid, err2 := strconv.Atoi(g.Gid); err2 == nil {
			_ = syscall.Chown(dir, 0, gid)
			_ = syscall.Chown(sockPath, 0, gid)
		}
	}
	_ = os.Chmod(dir, 0o750)
	_ = os.Chmod(sockPath, 0o660)
	logf("control socket listening at %s (group %s, mode 0660)", sockPath, group)
	return ln, nil
}

func acceptLoop(ln net.Listener, st *SharedState) {
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleConn(c, st)
	}
}

func handleConn(c net.Conn, st *SharedState) {
	defer c.Close()
	dec := json.NewDecoder(c)
	var r Req
	if err := dec.Decode(&r); err != nil {
		_ = json.NewEncoder(c).Encode(Resp{Ok: false, Msg: err.Error()})
		return
	}
	switch r.Cmd {
	case "set":
		st.mu.Lock()
		defer st.mu.Unlock()
		if r.Max < st.cfg.ConservationThreshold || r.Max > 100 {
			_ = json.NewEncoder(c).Encode(Resp{Ok: false, Msg: fmt.Sprintf("max must be %.1f..100", st.cfg.ConservationThreshold)})
			return
		}

		// Handle time parameter
		if r.Time != "" && r.Time != "now" {
			targetTime, err := parseTimeString(r.Time)
			if err != nil {
				_ = json.NewEncoder(c).Encode(Resp{Ok: false, Msg: fmt.Sprintf("invalid time format: %v", err)})
				return
			}
			st.cfg.TargetTime = &targetTime
		} else {
			// Time is "now" or not specified - immediate mode
			st.cfg.TargetTime = nil
		}

		st.cfg.MaxPercent = r.Max
		st.cfg.LevelReached = false // Reset level reached on new configuration

		if r.Auto != nil {
			st.cfg.Auto = *r.Auto
		}

		timeStr := "now"
		if st.cfg.TargetTime != nil {
			timeStr = st.cfg.TargetTime.Format("15:04")
		}

		_ = json.NewEncoder(c).Encode(Resp{Ok: true, Max: st.cfg.MaxPercent, Time: timeStr, Auto: st.cfg.Auto})
	case "get", "status":
		st.mu.Lock()
		timeStr := "now"
		if st.cfg.TargetTime != nil {
			timeStr = st.cfg.TargetTime.Format("15:04")
		}
		resp := Resp{
			Ok:    true,
			Max:   st.cfg.MaxPercent,
			Pct:   st.pct,
			State: stateString(st.bstate),
			Cons:  st.cons,
			Time:  timeStr,
			Auto:  st.cfg.Auto,
		}
		st.mu.Unlock()
		_ = json.NewEncoder(c).Encode(resp)
	default:
		_ = json.NewEncoder(c).Encode(Resp{Ok: false, Msg: "unknown cmd"})
	}
}

func stateString(s BatteryState) string {
	switch s {
	case BatteryStateCharging:
		return "charging"
	case BatteryStateDischarge:
		return "discharging"
	case BatteryStateFull:
		return "full"
	case BatteryStateEmpty:
		return "empty"
	case BatteryStatePending:
		return "pending"
	default:
		return "unknown"
	}
}

func findDisplayBattery(ctx context.Context, conn *dbus.Conn) (dbus.ObjectPath, error) {
	obj := conn.Object("org.freedesktop.UPower", dbus.ObjectPath("/org/freedesktop/UPower"))
	var path dbus.ObjectPath
	if err := obj.CallWithContext(ctx, "org.freedesktop.UPower.GetDisplayDevice", 0).Store(&path); err != nil {
		return "", fmt.Errorf("GetDisplayDevice: %w", err)
	}
	return path, nil
}

func isExternalDisplayConnected() (bool, error) {
	dirs, err := filepath.Glob("/sys/class/drm/*/status")
	if err != nil {
		return false, err
	}
	for _, statusFile := range dirs {
		dir := filepath.Base(filepath.Dir(statusFile))
		// skip internal displays
		if strings.Contains(dir, "eDP") || strings.Contains(dir, "LVDS") || strings.Contains(dir, "DSI") {
			continue
		}

		b, err := os.ReadFile(statusFile)
		if err != nil {
			continue
		}
		if strings.HasPrefix(string(b), "connected") {
			return true, nil
		}
	}
	return false, nil
}

func readUPower(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath) (percent float64, state BatteryState, err error) {
	obj := conn.Object("org.freedesktop.UPower", path)
	var variant dbus.Variant
	if err = obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.UPower.Device", "Percentage").Store(&variant); err != nil {
		return 0, 0, fmt.Errorf("get Percentage: %w", err)
	}
	p, ok := variant.Value().(float64)
	if !ok {
		return 0, 0, errors.New("percentage not float64")
	}
	var variant2 dbus.Variant
	if err = obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.UPower.Device", "State").Store(&variant2); err != nil {
		return 0, 0, fmt.Errorf("get State: %w", err)
	}
	switch v := variant2.Value().(type) {
	case uint32:
		return p, BatteryState(v), nil
	case uint64:
		return p, BatteryState(uint32(v)), nil
	default:
		return p, 0, errors.New("state not uint")
	}
}

func findConservationNode() (string, error) {
	candidates := []string{
		"/sys/bus/platform/drivers/ideapad_acpi/VPC2004:00/conservation_mode",
	}
	if matches, _ := filepath.Glob("/sys/bus/platform/drivers/ideapad_acpi/VPC????:??/conservation_mode"); len(matches) > 0 {
		candidates = append(candidates, matches...)
	}
	filepath.WalkDir("/sys/bus/platform/drivers/ideapad_acpi", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(path) == "conservation_mode" {
			candidates = append(candidates, path)
		}
		return nil
	})
	seen := make(map[string]struct{})
	best := ""
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if best == "" || len(p) < len(best) {
				best = p
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("conservation_mode not found under /sys/bus/platform/drivers/ideapad_acpi; ensure ideapad_laptop is loaded and the device exposes the knob")
	}
	return best, nil
}

func readConservation(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "1" {
		return 1, nil
	}
	return 0, nil
}

func writeConservation(path string, v int) error {
	if v != 0 && v != 1 {
		return fmt.Errorf("invalid conservation value %d", v)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	data := []byte(strconv.Itoa(v) + "\n")
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func parseTimeString(timeStr string) (time.Time, error) {
	if timeStr == "now" {
		return time.Now(), nil
	}

	// Parse HH:MM format
	now := time.Now()
	t, err := time.Parse("15:04", timeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("time must be in HH:MM format, got %s", timeStr)
	}

	// Set the date to today but with the specified time
	target := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())

	// If target time is in the past (or very close), assume user means tomorrow
	if target.Before(now.Add(time.Minute)) {
		target = target.Add(24 * time.Hour)
	}

	return target, nil
}

func logf(f string, a ...any) {
	ts := time.Now().Format(time.RFC3339)
	fmt.Printf("%s conservationd: %s\n", ts, fmt.Sprintf(f, a...))
}

func exitErr(err error) {
	fmt.Fprintf(os.Stderr, "conservationd: %v\n", err)
	os.Exit(1)
}
