// SPDX-License-Identifier: MIT
// conservationctl: Non-root CLI client for conservationd.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
)

type Req struct {
	Cmd  string  `json:"cmd"`
	Max  float64 `json:"max,omitempty"`
	Time string  `json:"time,omitempty"`
}
type Resp struct {
	Ok    bool    `json:"ok"`
	Msg   string  `json:"msg,omitempty"`
	Max   float64 `json:"max,omitempty"`
	Pct   float64 `json:"pct,omitempty"`
	State string  `json:"state,omitempty"`
	Cons  int     `json:"cons,omitempty"`
	Time  string  `json:"time,omitempty"`
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	sock := flag.String("sock", "/run/conservationd/conservationd.sock", "control socket path")
	doSet := flag.Bool("set", false, "set thresholds")
	max := flag.Float64("max", 80, "target maximum percentage (80..100)")
	timeFlag := flag.String("time", "", "target time in HH:MM format for scheduled charging (defaults to 'now')")
	status := flag.Bool("status", false, "show current status")
	flag.Parse()

	if *showVersion {
		fmt.Printf("conservationctl %s (commit %s, built %s) %s/%s\n", version, commit, date, runtime.GOOS, runtime.GOARCH)
        os.Exit(0)
    }

	// Handle time parameter
	timeValue := *timeFlag
	if timeValue == "" {
		timeValue = "now"
	}

	var req Req
	switch {
	case *doSet:
		req = Req{Cmd: "set", Max: *max, Time: timeValue}
	case *status:
		req = Req{Cmd: "status"}
	default:
		req = Req{Cmd: "get"}
	}

	c, err := net.Dial("unix", *sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer c.Close()

	if err := json.NewEncoder(c).Encode(req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var resp Resp
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !resp.Ok {
		fmt.Fprintf(os.Stderr, "error: %s\n", resp.Msg)
		os.Exit(1)
	}
	switch req.Cmd {
	case "set":
		fmt.Printf("max=%.1f time=%s\n", resp.Max, resp.Time)
	case "status", "get":
		fmt.Printf("pct=%.1f state=%s cons=%d max=%.1f time=%s\n", resp.Pct, resp.State, resp.Cons, resp.Max, resp.Time)
	}
}

// Version metadata injected at build time via -ldflags
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)
