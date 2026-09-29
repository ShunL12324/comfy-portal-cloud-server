// Package aria2 is a small JSON-RPC client for a long-lived aria2c.
//
// The RPC is what makes real progress possible: tellStatus reports exact
// completedLength, totalLength and downloadSpeed per file. aria2 also owns
// retries, resume and the concurrency limit.
package aria2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Client talks to aria2's RPC endpoint.
type Client struct {
	URL    string
	Secret string
	HTTP   *http.Client
}

func New(port int, secret string) *Client {
	return &Client{
		URL:    fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port),
		Secret: secret,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Args are the aria2c flags the supervisor runs it with.
func Args(port int, secret string, maxConcurrent int) []string {
	return []string{
		"--enable-rpc",
		fmt.Sprintf("--rpc-listen-port=%d", port),
		"--rpc-secret=" + secret,
		// Loopback only. This is an internal control channel and the box has a
		// public IP.
		"--rpc-listen-all=false",
		fmt.Sprintf("--max-concurrent-downloads=%d", maxConcurrent),
		"--continue=true",
		"--file-allocation=none",
		"--max-tries=0",
		"--retry-wait=5",
		"--split=16",
		"--max-connection-per-server=16",
		"--min-split-size=1M",
		"--summary-interval=0",
		"--console-log-level=warn",
	}
}

// Error is an error reported by aria2 itself, as opposed to a transport one.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// ErrGIDNotFound means aria2 no longer knows the download, which happens when
// it was restarted.
var ErrGIDNotFound = errors.New("aria2: gid not found")

func (c *Client) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "cpd",
		"method":  method,
		"params":  append([]any{"token:" + c.Secret}, params...),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("aria2: bad response (HTTP %d): %w", resp.StatusCode, err)
	}
	if body.Error != nil {
		if bytes.Contains([]byte(body.Error.Message), []byte("is not found")) {
			return nil, ErrGIDNotFound
		}
		return nil, &Error{Message: body.Error.Message}
	}
	return body.Result, nil
}

// WaitReady polls until the RPC answers.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := c.call(ctx, "aria2.getVersion"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("aria2 RPC did not come up")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Options are the per-download aria2 options this supervisor uses.
type Options struct {
	Dir     string
	Out     string
	Headers []string
}

func (c *Client) AddURI(ctx context.Context, uri string, o Options) (string, error) {
	opts := map[string]any{"dir": o.Dir, "out": o.Out}
	if len(o.Headers) > 0 {
		opts["header"] = o.Headers
	}
	raw, err := c.call(ctx, "aria2.addUri", []string{uri}, opts)
	if err != nil {
		return "", err
	}
	var gid string
	return gid, json.Unmarshal(raw, &gid)
}

// Status is one download's progress. aria2 sends every number as a string.
type Status struct {
	State        string
	Completed    int64
	Total        int64
	Speed        int64
	ErrorMessage string
}

func (c *Client) TellStatus(ctx context.Context, gid string) (Status, error) {
	raw, err := c.call(ctx, "aria2.tellStatus", gid)
	if err != nil {
		return Status{}, err
	}
	var r struct {
		Status          string `json:"status"`
		CompletedLength string `json:"completedLength"`
		TotalLength     string `json:"totalLength"`
		DownloadSpeed   string `json:"downloadSpeed"`
		ErrorMessage    string `json:"errorMessage"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Status{}, err
	}
	n := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
	return Status{
		State:        r.Status,
		Completed:    n(r.CompletedLength),
		Total:        n(r.TotalLength),
		Speed:        n(r.DownloadSpeed),
		ErrorMessage: r.ErrorMessage,
	}, nil
}
