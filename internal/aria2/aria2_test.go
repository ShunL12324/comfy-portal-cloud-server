package aria2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fake(t *testing.T, handle func(method string, params []json.RawMessage) (any, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var secret string
		_ = json.Unmarshal(req.Params[0], &secret)
		if secret != "token:s3cret" {
			t.Errorf("secret = %q", secret)
		}
		result, errMsg := handle(req.Method, req.Params[1:])
		out := map[string]any{"jsonrpc": "2.0", "id": "cpd"}
		if errMsg != "" {
			out["error"] = map[string]any{"message": errMsg}
		} else {
			out["result"] = result
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Secret: "s3cret", HTTP: srv.Client()}
}

func TestTellStatusParsesStringNumbers(t *testing.T) {
	c := fake(t, func(method string, _ []json.RawMessage) (any, string) {
		return map[string]string{"status": "active", "completedLength": "10", "totalLength": "100", "downloadSpeed": "5"}, ""
	})
	st, err := c.TellStatus(context.Background(), "g")
	if err != nil || st.Completed != 10 || st.Total != 100 || st.Speed != 5 || st.State != "active" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestGIDNotFound(t *testing.T) {
	c := fake(t, func(string, []json.RawMessage) (any, string) { return nil, "GID abc is not found" })
	if _, err := c.TellStatus(context.Background(), "abc"); err != ErrGIDNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestAddURIPassesOptions(t *testing.T) {
	c := fake(t, func(method string, params []json.RawMessage) (any, string) {
		var opts map[string]any
		_ = json.Unmarshal(params[1], &opts)
		if method != "aria2.addUri" || opts["dir"] != "/d" || opts["out"] != "f" || opts["header"] == nil {
			t.Errorf("method=%s opts=%v", method, opts)
		}
		return "gid1", ""
	})
	gid, err := c.AddURI(context.Background(), "http://x", Options{Dir: "/d", Out: "f", Headers: []string{"A: b"}})
	if err != nil || gid != "gid1" {
		t.Fatal(gid, err)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1", Secret: "x", HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	if err := c.WaitReady(context.Background(), 300*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}
