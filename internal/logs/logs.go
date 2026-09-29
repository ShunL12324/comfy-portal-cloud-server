// Package logs reads the tail of log files and sets up the supervisor's own logger.
package logs

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Streams are the log files the API can serve, by name.
var Streams = []string{"supervisor", "comfyui", "ollama", "aria2"}

// MaxTail bounds one request so a huge N cannot make the box read gigabytes.
const MaxTail = 5000

// Tail returns the last n lines of the file, reading only from the end. A
// missing file is an empty log, not an error: the stream simply has not
// written anything yet.
func Tail(path string, n int) (string, error) {
	n = max(1, min(n, MaxTail))
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	const chunk = 64 * 1024
	var buf []byte
	pos := info.Size()
	for pos > 0 && bytes.Count(buf, []byte("\n")) <= n {
		size := int64(chunk)
		if pos < size {
			size = pos
		}
		pos -= size
		part := make([]byte, size)
		if _, err := f.ReadAt(part, pos); err != nil && err != io.EOF {
			return "", err
		}
		buf = append(part, buf...)
	}
	lines := strings.SplitAfter(string(buf), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, ""), nil
}

// Setup makes slog write to w (already redacting) in a compact text format.
func Setup(w io.Writer) {
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().Format("15:04:05"))
			}
			return a
		},
	})))
}
