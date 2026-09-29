// Package redact strips secrets from text before it is logged or served.
//
// The API port is public and aria2 echoes request headers when a download
// fails, so this is not optional.
package redact

import (
	"io"
	"strings"
	"sync"
)

// Redactor replaces every registered secret with "***".
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

func New(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add registers more secrets. Values shorter than 8 bytes are ignored: they
// would mangle ordinary text and are not worth protecting.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) >= 8 {
			r.secrets = append(r.secrets, s)
		}
	}
}

func (r *Redactor) String(text string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.secrets {
		text = strings.ReplaceAll(text, s, "***")
	}
	return text
}

// Writer wraps w so everything written through it is redacted. Writes are
// treated as whole lines, which is how log/slog emits them.
func (r *Redactor) Writer(w io.Writer) io.Writer { return redactWriter{r, w} }

type redactWriter struct {
	r *Redactor
	w io.Writer
}

func (rw redactWriter) Write(p []byte) (int, error) {
	if _, err := rw.w.Write([]byte(rw.r.String(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
