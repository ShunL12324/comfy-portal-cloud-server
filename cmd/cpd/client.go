package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// The subcommands talk to the local supervisor over loopback. They exist so a
// remote shell (the app reaches the box over SSH) can ask questions without
// hand-writing curl and quoting a bearer token.

func baseURL() string {
	port := os.Getenv("CP_PORT")
	if port == "" {
		port = "8189"
	}
	return "http://127.0.0.1:" + port
}

func get(path string, timeout time.Duration) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL()+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if t := os.Getenv("CP_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("supervisor is not answering: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func health() error {
	_, code, err := get("/v2/health", 3*time.Second)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("health returned HTTP %d", code)
	}
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	body, code, err := get("/v2/snapshot", 10*time.Second)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	if *asJSON {
		fmt.Println(string(body))
		return nil
	}
	var snap struct {
		Version string `json:"version"`
		state.Snapshot
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		return err
	}
	fmt.Printf("cpd %s  phase=%s  elapsed=%ds\n", snap.Version, snap.Phase, snap.Elapsed)
	if snap.Error != nil {
		fmt.Printf("error: %s: %s\n", snap.Error.Code, snap.Error.Message)
	}
	t := snap.Totals
	fmt.Printf("models: %d, %.1f / %.1f GiB\n", len(snap.Models), float64(t.Completed)/(1<<30), float64(t.Bytes)/(1<<30))
	for _, m := range snap.Models {
		fmt.Printf("  %-8s %s/%s\n", m.State, m.Folder, m.Name)
	}
	for _, s := range snap.Services {
		fmt.Printf("service %-8s %s (restarts %d)\n", s.Name, s.State, s.Restarts)
	}
	return nil
}

// report prints the launch's timings: how long each phase and step took and
// how fast each model came down.
func report() error {
	body, code, err := get("/v2/snapshot", 10*time.Second)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var snap state.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return err
	}
	for _, line := range snap.Report() {
		fmt.Println(line)
	}
	return nil
}

func logsCmd(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	n := fs.Int("n", 200, "lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stream := "supervisor"
	if fs.NArg() > 0 {
		stream = fs.Arg(0)
	}
	body, code, err := get(fmt.Sprintf("/v2/logs/%s?tail=%d", stream, *n), 15*time.Second)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
	return nil
}
