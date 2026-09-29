// Package install prepares the box: directory links, extensions, Ollama and
// the optional direct SSH service.
package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// Runner runs a command and returns its combined output. Tests substitute it.
type Runner func(ctx context.Context, dir string, argv ...string) (string, error)

// Exec is the real Runner.
func Exec(ctx context.Context, dir string, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// LinkIntoComfy points ComfyUI's mutable directories at the workspace.
//
// The image ships the code and the venv; the workspace is the only disk the
// host actually mounts, and the only one sized for models.
func LinkIntoComfy(workspace, comfyDir string) error {
	for _, name := range []string{"models", "input", "output", "custom_nodes"} {
		target := filepath.Join(workspace, name)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		link := filepath.Join(comfyDir, name)
		info, err := os.Lstat(link)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			continue
		case err == nil && info.IsDir():
			// The image's own custom_nodes (Manager, the endpoint) have to come
			// along, or the app loses /cpe/* the moment this runs.
			entries, err := os.ReadDir(link)
			if err != nil {
				return err
			}
			for _, e := range entries {
				dst := filepath.Join(target, e.Name())
				if _, err := os.Lstat(dst); err == nil {
					continue
				}
				if err := move(filepath.Join(link, e.Name()), dst); err != nil {
					return err
				}
			}
			if err := os.RemoveAll(link); err != nil {
				return err
			}
		case err == nil:
			return fmt.Errorf("%s exists and is not a directory", link)
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}

// renameFn is os.Rename; tests replace it to simulate a cross-device move.
var renameFn = os.Rename

// move renames src to dst, falling back to copy-then-delete when they are on
// different filesystems. That is the normal case on vast: /workspace is a
// separate mount from the image's layer, and rename(2) cannot cross it.
//
// The copy lands in dst+".partial" and is renamed into place only when
// complete, so a crash part-way never leaves a half-populated dst that the
// "already there, skip it" check would then trust.
func move(src, dst string) error {
	err := renameFn(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	partial := dst + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return err
	}
	if err := copyTree(src, partial); err != nil {
		_ = os.RemoveAll(partial)
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := os.Rename(partial, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		}
		return nil // sockets, devices: not part of a ComfyUI checkout
	})
}

func copyFile(src, dst string, perm os.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// Extensions clones each extension and installs its requirements. It stops at
// the first failure. Extensions already on disk are not re-cloned. It
// reports whether anything new was installed, since ComfyUI must restart to
// load it.
func Extensions(ctx context.Context, st *state.State, run Runner, workspace, comfyDir string, urls []string) (installed bool, err error) {
	root := filepath.Join(workspace, "custom_nodes")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false, err
	}
	pip := filepath.Join(comfyDir, "venv", "bin", "pip")
	for _, url := range urls {
		name := manifest.ExtensionName(url)
		id := "extension:" + url
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			st.Step(id, state.StepDone, name)
			continue
		}
		st.Step(id, state.StepRunning, name)
		if out, err := run(ctx, "", "git", "clone", "--depth", "1", "--", url, path); err != nil {
			_ = os.RemoveAll(path) // a half-clone would be skipped next time
			st.Step(id, state.StepFailed, tail(out))
			return installed, fmt.Errorf("could not clone extension %s", name)
		}
		req := filepath.Join(path, "requirements.txt")
		if _, err := os.Stat(req); err == nil {
			if _, perr := os.Stat(pip); perr == nil {
				if out, err := run(ctx, "", pip, "install", "-q", "-r", req); err != nil {
					st.Step(id, state.StepFailed, tail(out))
					return installed, fmt.Errorf("could not install dependencies for %s", name)
				}
			}
		}
		installed = true
		st.Step(id, state.StepDone, name)
		slog.Info("installed extension", "name", name)
	}
	return installed, nil
}

// PullOllama pulls each model through a running ollama server.
func PullOllama(ctx context.Context, st *state.State, run Runner, models []string) error {
	for _, model := range models {
		id := "ollama:" + model
		st.Step(id, state.StepRunning, "")
		if out, err := run(ctx, "", "ollama", "pull", model); err != nil {
			st.Step(id, state.StepFailed, tail(out))
			return fmt.Errorf("could not pull Ollama model %s", model)
		}
		st.Step(id, state.StepDone, "")
	}
	return nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}

// ValidateSSHKey accepts only a single-line ssh-rsa or ssh-ed25519 public key.
func ValidateSSHKey(key string) error {
	key = strings.TrimSpace(key)
	fields := strings.Fields(key)
	if len(fields) < 2 || (fields[0] != "ssh-rsa" && fields[0] != "ssh-ed25519") || strings.ContainsAny(key, "\r\n") {
		return errors.New("invalid SSH public key")
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return errors.New("invalid SSH public key")
	}
	return nil
}

// StartSSH authorises key and starts a key-only sshd. Vast supplies its own
// SSH service; RunPod uses this image's.
func StartSSH(ctx context.Context, run Runner, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if err := ValidateSSHKey(key); err != nil {
		return err
	}
	if err := os.MkdirAll("/root/.ssh", 0o700); err != nil {
		return err
	}
	if err := os.Chmod("/root/.ssh", 0o700); err != nil {
		return err
	}
	if err := os.WriteFile("/root/.ssh/authorized_keys", []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll("/run/sshd", 0o755); err != nil {
		return err
	}
	if out, err := run(ctx, "", "ssh-keygen", "-A"); err != nil {
		return fmt.Errorf("ssh-keygen: %s", tail(out))
	}
	out, err := run(ctx, "", "/usr/sbin/sshd",
		"-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
		"-o", "PermitRootLogin=prohibit-password", "-o", "PubkeyAuthentication=yes", "-o", "UsePAM=no")
	if err != nil {
		return fmt.Errorf("sshd: %s", tail(out))
	}
	return nil
}
