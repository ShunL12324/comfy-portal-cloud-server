// Package manifest defines what a launch asks the supervisor to install.
package manifest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Manifest is the launch's models, extensions and Ollama models.
type Manifest struct {
	Version      int      `json:"version,omitempty"`
	Models       []Model  `json:"models"`
	Extensions   []string `json:"extensions"`
	OllamaModels []string `json:"ollamaModels"`
}

// Model is one file to download into ComfyUI's models directory.
type Model struct {
	URL       string `json:"url"`
	Folder    string `json:"folder,omitempty"`
	Type      string `json:"type,omitempty"` // legacy alias for Folder
	Filename  string `json:"filename,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// Resolved is a Model with its defaults applied.
type Resolved struct {
	Model
	ID     string
	Folder string
	Name   string
}

// Key is the legacy "<folder>/<name>" identity the v1 API used.
func (r Resolved) Key() string { return r.Folder + "/" + r.Name }

// Decode parses the base64 JSON that CP_MANIFEST carries.
//
// It is base64 because vast takes container env as one docker-flag string
// split on whitespace: a multi-line value silently loses everything after its
// first line, and the same value then has to survive /etc/environment.
func Decode(b64 string) (Manifest, error) {
	if b64 == "" {
		return Manifest{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Manifest{}, fmt.Errorf("CP_MANIFEST is not valid base64: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("CP_MANIFEST is not valid JSON: %w", err)
	}
	return m, m.Validate()
}

// Validate rejects entries that cannot possibly work, before any of them
// costs a download.
func (m Manifest) Validate() error {
	for i, model := range m.Models {
		u, err := url.Parse(model.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("models[%d]: url must be an absolute http(s) URL", i)
		}
		r := model.Resolve()
		if !safeSegment(r.Folder) && !safeRelPath(r.Folder) {
			return fmt.Errorf("models[%d]: folder %q is not a plain relative path", i, r.Folder)
		}
		if !safeSegment(r.Name) {
			return fmt.Errorf("models[%d]: filename %q is not a plain file name", i, r.Name)
		}
	}
	for i, ext := range m.Extensions {
		u, err := url.Parse(ext)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("extensions[%d]: must be an http(s) git URL", i)
		}
	}
	for i, name := range m.OllamaModels {
		if strings.TrimSpace(name) == "" || strings.HasPrefix(name, "-") {
			return fmt.Errorf("ollamaModels[%d]: invalid model name", i)
		}
	}
	return nil
}

// Resolve applies defaults: folder falls back to the legacy type, then to
// "checkpoints"; the filename falls back to the URL's last path segment.
func (m Model) Resolve() Resolved {
	folder := m.Folder
	if folder == "" {
		folder = m.Type
	}
	if folder == "" {
		folder = "checkpoints"
	}
	name := m.Filename
	if name == "" {
		if u, err := url.Parse(m.URL); err == nil {
			name = path.Base(u.Path)
		}
	}
	return Resolved{Model: m, ID: ID(folder, name), Folder: folder, Name: name}
}

// ID is a stable, URL-safe identity for a model: the same folder and file
// always give the same ID, and it never contains a slash.
func ID(folder, name string) string {
	sum := sha256.Sum256([]byte(folder + "/" + name))
	return hex.EncodeToString(sum[:6])
}

func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`) && !strings.ContainsRune(s, 0)
}

// safeRelPath allows nested model folders such as "loras/flux" but nothing
// that could climb out of the models directory.
func safeRelPath(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.ContainsAny(s, `\`) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if !safeSegment(part) {
			return false
		}
	}
	return true
}

// ExtensionName is the directory an extension clones into.
func ExtensionName(u string) string {
	return strings.TrimSuffix(path.Base(strings.TrimRight(u, "/")), ".git")
}
