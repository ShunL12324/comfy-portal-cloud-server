package manifest

import (
	"encoding/base64"
	"testing"
)

func encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestDecodeEmpty(t *testing.T) {
	m, err := Decode("")
	if err != nil || len(m.Models) != 0 {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

func TestDecodeAppliesDefaults(t *testing.T) {
	m, err := Decode(encode(`{"version":1,"models":[{"url":"https://h.example/a/b/x.safetensors","type":"loras"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Models[0].Resolve()
	if r.Folder != "loras" || r.Name != "x.safetensors" || r.Key() != "loras/x.safetensors" {
		t.Fatalf("%+v", r)
	}
	if ID("loras", "x.safetensors") != r.ID {
		t.Fatal("ID not stable")
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"not base64":     "%%%",
		"not json":       encode("nope"),
		"relative url":   encode(`{"models":[{"url":"/x"}]}`),
		"file scheme":    encode(`{"models":[{"url":"file:///etc/passwd"}]}`),
		"path traversal": encode(`{"models":[{"url":"https://h/x","folder":"../etc"}]}`),
		"filename slash": encode(`{"models":[{"url":"https://h/x","filename":"a/b"}]}`),
		"bad extension":  encode(`{"extensions":["git@github.com:a/b"]}`),
		"ollama flag":    encode(`{"ollamaModels":["--help"]}`),
	}
	for name, in := range cases {
		if _, err := Decode(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNestedFolderAllowed(t *testing.T) {
	if _, err := Decode(encode(`{"models":[{"url":"https://h/x.bin","folder":"loras/flux"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionName(t *testing.T) {
	if got := ExtensionName("https://github.com/u/ComfyUI-Thing.git/"); got != "ComfyUI-Thing" {
		t.Fatal(got)
	}
}
