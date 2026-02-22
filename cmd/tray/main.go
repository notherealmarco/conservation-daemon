package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/getlantern/systray"
	"github.com/godbus/dbus/v5"
	"github.com/ncruces/zenity"
)

// Req and Resp matched to daemon IPC protocol
type Req struct {
	Cmd  string  `json:"cmd"`
	Max  float64 `json:"max,omitempty"`
	Time string  `json:"time,omitempty"`
	Auto *bool   `json:"auto,omitempty"`
}

type Resp struct {
	Ok    bool    `json:"ok"`
	Msg   string  `json:"msg,omitempty"`
	Max   float64 `json:"max,omitempty"`
	Pct   float64 `json:"pct,omitempty"`
	State string  `json:"state,omitempty"`
	Cons  int     `json:"cons,omitempty"`
	Time  string  `json:"time,omitempty"`
	Auto  bool    `json:"auto,omitempty"`
}

var sockPath string
var currentState Resp
var refreshCh = make(chan struct{}, 1)

// generateIcon creates a battery-shaped icon with color reflecting state.
// Gray = unplugged/idle, Green = charging, Blue = conservation enabled.
func generateIcon(plugged bool, charging bool, consEnabled bool) []byte {
	rect := image.Rect(0, 0, 64, 64)
	img := image.NewRGBA(rect)

	c := color.RGBA{80, 80, 80, 255} // Gray: unplugged or idle
	if plugged && consEnabled {
		c = color.RGBA{0, 150, 255, 255} // Blue: conservation on
	} else if plugged && charging {
		c = color.RGBA{0, 200, 80, 255} // Green: charging
	} else if plugged {
		c = color.RGBA{200, 200, 200, 255} // Light gray: plugged but idle
	}

	// Battery body
	for y := 16; y < 48; y++ {
		for x := 10; x < 54; x++ {
			img.Set(x, y, c)
		}
	}
	// Battery tip (positive terminal)
	for y := 24; y < 40; y++ {
		for x := 54; x < 58; x++ {
			img.Set(x, y, c)
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func doIPC(req Req) (*Resp, error) {
	c, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var resp Resp
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.Ok {
		return nil, fmt.Errorf("daemon error: %s", resp.Msg)
	}
	return &resp, nil
}

func isACPluggedIn() bool {
	conn, err := dbus.SystemBus()
	if err != nil {
		return false
	}
	defer conn.Close()

	obj := conn.Object("org.freedesktop.UPower", dbus.ObjectPath("/org/freedesktop/UPower"))
	variant, err := obj.GetProperty("org.freedesktop.UPower.OnBattery")
	if err != nil {
		return false
	}
	onBattery, ok := variant.Value().(bool)
	if !ok {
		return false
	}
	return !onBattery
}

func main() {
	flag.StringVar(&sockPath, "sock", "/run/conservationd/conservationd.sock", "daemon socket path")
	flag.Parse()

	systray.Run(onReady, onExit)
}

func onExit() {}

func onReady() {
	icon := generateIcon(false, false, false)
	systray.SetIcon(icon)
	systray.SetTitle("Conservation")
	systray.SetTooltip("Battery Conservation Daemon")

	mStatus := systray.AddMenuItem("Status: connecting...", "Current daemon status")
	mStatus.Disable()

	systray.AddSeparator()
	mConfigure := systray.AddMenuItem("Configure Conservation", "Set Max % and Target Time")
	mToggleAuto := systray.AddMenuItemCheckbox("Auto Mode (Enable on external display)", "Toggle display-based auto mode", false)
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit Tray", "Exit tray applet")

	// Polling goroutine: updates icon, status text, and auto checkbox
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			pluggedIn := isACPluggedIn()

			resp, err := doIPC(Req{Cmd: "status"})
			if err != nil {
				mStatus.SetTitle("Status: daemon unreachable")
				systray.SetTooltip("Conservation: daemon unreachable")
				systray.SetIcon(generateIcon(false, false, false))
			} else {
				currentState = *resp

				systray.SetIcon(generateIcon(pluggedIn, resp.State == "charging", resp.Cons > 0))

				consStr := "OFF"
				if resp.Cons > 0 {
					consStr = "ON"
				}
				statusStr := fmt.Sprintf("%.0f%% | Max: %.0f%% | Time: %s | Cons: %s",
					resp.Pct, resp.Max, resp.Time, consStr)
				mStatus.SetTitle(statusStr)
				systray.SetTooltip(fmt.Sprintf("Battery: %.0f%% — Conservation %s", resp.Pct, consStr))

				if resp.Auto {
					mToggleAuto.Check()
				} else {
					mToggleAuto.Uncheck()
				}
			}

			select {
			case <-ticker.C:
			case <-refreshCh:
			}
		}
	}()

	// Event handler goroutine
	go func() {
		for {
			select {
			case <-mConfigure.ClickedCh:
				configureClicked()
			case <-mToggleAuto.ClickedCh:
				toggleAutoMode()
			case <-mQuit.ClickedCh:
				systray.Quit()
				os.Exit(0)
			}
		}
	}()
}

func configureClicked() {
	fmt.Fprintf(os.Stderr, "configure clicked: cons=%d max=%.1f\n", currentState.Cons, currentState.Max)
	if currentState.Cons > 0 {
		// Conservation is ON - let user set a charge target (disable conservation temporarily)
		maxStr, err := zenity.Entry("Enter target maximum battery percentage (80-100):",
			zenity.Title("Configure Conservation"),
			zenity.EntryText("100"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "zenity entry (max) error: %v\n", err)
			return
		}

		maxFloat, err := strconv.ParseFloat(maxStr, 64)
		if err != nil || maxFloat < 80 || maxFloat > 100 {
			zenity.Error("Invalid percentage. Must be between 80 and 100.",
				zenity.Title("Error"))
			return
		}

		timeStr, err := zenity.Entry("Enter target time (HH:MM format, or 'now'):",
			zenity.Title("Configure Schedule"),
			zenity.EntryText("now"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "zenity entry (time) error: %v\n", err)
			return
		}

		doIPC(Req{Cmd: "set", Max: maxFloat, Time: timeStr})
		select {
		case refreshCh <- struct{}{}:
		default:
		}
		return
	}

	// Conservation is OFF - offer to reset back to default (re-enable conservation at 80%)
	err := zenity.Question(
		"Conservation mode is currently disabled.\nRe-enable it? (Max: 80%, immediate)",
		zenity.Title("Enable Conservation Mode"),
		zenity.QuestionIcon,
	)
	if err == nil {
		doIPC(Req{Cmd: "set", Max: 80, Time: "now"})
		select {
		case refreshCh <- struct{}{}:
		default:
		}
	}
}

func toggleAutoMode() {
	newAuto := !currentState.Auto
	doIPC(Req{Cmd: "set", Max: currentState.Max, Time: currentState.Time, Auto: &newAuto})
	select {
	case refreshCh <- struct{}{}:
	default:
	}
}
