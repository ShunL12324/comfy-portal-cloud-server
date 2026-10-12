package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "x.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTail(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	got, err := Tail(write(t, b.String()), 3)
	if err != nil || got != "line 19998\nline 19999\nline 20000\n" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTailShortFileAndNoTrailingNewline(t *testing.T) {
	got, _ := Tail(write(t, "a\nb"), 10)
	if got != "a\nb" {
		t.Fatalf("%q", got)
	}
}

func TestTailMissingFile(t *testing.T) {
	got, err := Tail(filepath.Join(t.TempDir(), "nope"), 5)
	if err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
}
