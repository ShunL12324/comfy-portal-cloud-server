package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// The v1 shape is parsed by the app, which lives in another repository. Point
// CP_APP_DIR at a checkout of it and this fails when a field the app declares
// as required stops being sent; without it the test is skipped.
func TestV1MatchesTheAppsTypeScriptClient(t *testing.T) {
	dir := os.Getenv("CP_APP_DIR")
	if dir == "" {
		t.Skip("CP_APP_DIR not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "src/services/cloud-supervisor.ts"))
	if err != nil {
		t.Fatal(err)
	}
	ts := string(raw)
	// `field?:` is optional by design, so only non-optional fields must always be sent.
	fieldsOf := func(name, until string) []string {
		start, end := strings.Index(ts, "export interface "+name), strings.Index(ts, until)
		var out []string
		for _, m := range regexp.MustCompile(`(?m)^ {2}(\w+):`).FindAllStringSubmatch(ts[start:end], -1) {
			out = append(out, m[1])
		}
		return out
	}

	r := newRig(t)
	r.st.PutModel(state.Model{ID: "1", Name: "x", Folder: "f", State: state.ModelDone})
	_, body := r.do(t, "GET", "/v1/status", "")
	var snap map[string]json.RawMessage
	decode(t, body, &snap)
	for _, f := range fieldsOf("SupervisorSnapshot", "export class SupervisorError") {
		if _, ok := snap[f]; !ok {
			t.Errorf("snapshot field %q is declared by the app but not sent", f)
		}
	}
	var models []map[string]any
	decode(t, string(snap["models"]), &models)
	for _, f := range fieldsOf("SupervisorModel", "export interface SupervisorSnapshot") {
		if _, ok := models[0][f]; !ok {
			t.Errorf("model field %q is declared by the app but not sent", f)
		}
	}
}
