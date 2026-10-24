package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// cmdHealthcheck probes the dashboard API's /api/health endpoint and
// exits 0 when the API answers healthy, 1 otherwise.
//
// It exists for container health checks: the API image is built FROM
// scratch, so there is no shell and no curl inside it. The binary is
// the only thing that can be executed, so the binary carries the probe.
//
//	alpacaruns healthcheck [--url http://127.0.0.1:8090] [--timeout 5s]
func cmdHealthcheck(args []string) int {
	var url string
	var timeout time.Duration
	fs := newFlagSet("healthcheck")
	fs.StringVar(&url, "url", "", "API base URL (default: http://127.0.0.1:$ALPACA_API_PORT or 8080)")
	fs.DurationVar(&timeout, "timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "healthcheck: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if url == "" {
		port := os.Getenv("ALPACA_API_PORT")
		if port == "" {
			port = "8080"
		}
		url = "http://127.0.0.1:" + port
	}
	url = strings.TrimSuffix(url, "/") + "/api/health"

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: unreadable body: %v\n", err)
		return 1
	}
	if !body.OK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s reported not ok\n", url)
		return 1
	}
	fmt.Println("ok")
	return 0
}

// newFlagSet builds a FlagSet with the project's standard error output.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
