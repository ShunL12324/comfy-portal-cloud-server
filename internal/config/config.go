// Package config reads the process environment once, at startup.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config is everything the supervisor takes from its environment.
type Config struct {
	Workspace string
	ComfyDir  string
	ComfyPort int
	Listen    string
	Token     string

	HFToken       string
	CivitaiAPIKey string
	SSHPublicKey  string
	ManifestB64   string

	Aria2Port   int
	Aria2Secret string

	// Downloading one file from one host tops out well below the box's uplink,
	// so several run at once, but not all of them or they compete for it.
	MaxDownloads int
	// No byte movement for this long means something is wedged.
	StallAfter time.Duration
	// How long ComfyUI gets to answer /system_stats after it is spawned.
	ComfyStartTimeout time.Duration
	// How long extension and Ollama installation may take in total.
	InstallTimeout time.Duration
	PollInterval   time.Duration

	// AllowNoAuth lets the API run without CP_TOKEN. Only for local development:
	// on a rented instance the port is on a public IP.
	AllowNoAuth bool
}

// FromEnv builds a Config from os.Getenv. Defaults match the runtime image.
func FromEnv() (Config, error) {
	get := os.Getenv
	c := Config{
		Workspace:     or(get("CP_WORKSPACE"), "/workspace"),
		ComfyDir:      or(get("COMFY_DIR"), "/opt/comfyui"),
		Token:         get("CP_TOKEN"),
		HFToken:       get("HF_TOKEN"),
		CivitaiAPIKey: get("CIVITAI_API_KEY"),
		SSHPublicKey:  get("CP_SSH_PUBLIC_KEY"),
		ManifestB64:   get("CP_MANIFEST"),
		Aria2Port:     6800,
		AllowNoAuth:   get("CP_ALLOW_NO_AUTH") == "1",
	}
	var err error
	if c.ComfyPort, err = intEnv("COMFY_PORT", 8188); err != nil {
		return c, err
	}
	port, err := intEnv("CP_PORT", 8189)
	if err != nil {
		return c, err
	}
	c.Listen = or(get("CP_LISTEN"), fmt.Sprintf(":%d", port))
	if c.MaxDownloads, err = intEnv("CP_MAX_DOWNLOADS", 4); err != nil {
		return c, err
	}
	stall, err := intEnv("CP_STALL_SECONDS", 300)
	if err != nil {
		return c, err
	}
	c.StallAfter = time.Duration(stall) * time.Second
	c.ComfyStartTimeout = 10 * time.Minute
	c.InstallTimeout = 30 * time.Minute
	c.PollInterval = 2 * time.Second

	secret := make([]byte, 18)
	if _, err := rand.Read(secret); err != nil {
		return c, err
	}
	c.Aria2Secret = base64.RawURLEncoding.EncodeToString(secret)

	if c.Token == "" && !c.AllowNoAuth {
		return c, fmt.Errorf("CP_TOKEN is required (set CP_ALLOW_NO_AUTH=1 for local development)")
	}
	return c, nil
}

// Secrets are the values that must never reach a log file or an HTTP response.
func (c Config) Secrets() []string {
	var out []string
	for _, s := range []string{c.Token, c.HFToken, c.CivitaiAPIKey, c.Aria2Secret} {
		if len(s) >= 8 {
			out = append(out, s)
		}
	}
	return out
}

func (c Config) StatePath() string { return filepath.Join(c.Workspace, "supervisor-state.json") }
func (c Config) LogPath(name string) string {
	return filepath.Join(c.Workspace, name+".log")
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func intEnv(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}
