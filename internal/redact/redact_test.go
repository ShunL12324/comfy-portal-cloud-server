package redact

import (
	"bytes"
	"testing"
)

func TestStringRemovesSecrets(t *testing.T) {
	r := New("super-secret-token", "short")
	got := r.String("Authorization: Bearer super-secret-token short")
	want := "Authorization: Bearer *** short"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	r := New("super-secret-token")
	n, err := r.Writer(&buf).Write([]byte("x super-secret-token y\n"))
	if err != nil || n != len("x super-secret-token y\n") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if buf.String() != "x *** y\n" {
		t.Fatalf("got %q", buf.String())
	}
}
