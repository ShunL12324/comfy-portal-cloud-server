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
	"slices"
	"strings"
	"sync"
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
			// The tree being copied is the image's own ComfyUI checkout, read at boot
			// before anything untrusted runs, so there is no one to race the walk.
			return os.Symlink(link, target) //nolint:gosec // G122: trusted source, see above
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

// cloneParallel bounds concurrent clones: each is mostly waiting on GitHub,
// but dozens at once would trip its rate limits.
const cloneParallel = 8

// Extensions clones the extensions that are not on disk yet, all at once, and
// installs their requirements. It reports whether anything new was installed,
// since ComfyUI must restart to load it.
//
// With uv in the venv (the image ships it) every requirements file goes to
// one resolver call: one download pass instead of one per extension, and a set
// of versions that satisfies all of them rather than whichever extension
// happened to install last. Extensions already on disk are included, which
// costs a resolve when nothing is missing and repairs one whose install was
// interrupted. If the joint install fails, each extension is installed on its
// own so the failure lands on the extension that caused it. Without uv it
// falls back to pip, one extension at a time.
//
// A clone or install failure fails the call, but only after every other
// extension has been installed, so no step is left running.
func Extensions(ctx context.Context, st *state.State, run Runner, workspace, comfyDir string, urls []string) (installed bool, err error) {
	root := filepath.Join(workspace, "custom_nodes")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false, err
	}
	if len(urls) == 0 {
		return false, nil
	}
	st.Step("extensions", state.StepRunning, "")

	type ext struct{ url, name, id, path string }
	var fresh, present []ext
	for _, url := range urls {
		name := manifest.ExtensionName(url)
		e := ext{url, name, "extension:" + url, filepath.Join(root, name)}
		if _, err := os.Stat(e.path); err == nil {
			st.Step(e.id, state.StepDone, name)
			present = append(present, e)
			continue
		}
		fresh = append(fresh, e)
	}

	failed := map[string]error{} // by id
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, cloneParallel)
	for _, e := range fresh {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st.Step(e.id, state.StepRunning, e.name)
			out, err := run(ctx, "", "git", "clone", "--depth", "1", "--recurse-submodules", "--shallow-submodules", "--", e.url, e.path)
			if err != nil {
				_ = os.RemoveAll(e.path) // a half-clone would be skipped next time
				st.Step(e.id, state.StepFailed, tail(out))
				mu.Lock()
				failed[e.id] = fmt.Errorf("could not clone extension %s", e.name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Requirements of everything that is on disk now.
	py := NewPython(comfyDir)
	var reqs []ext
	for _, e := range append(present, fresh...) {
		if failed[e.id] != nil || (!py.UV && slices.Contains(present, e)) {
			continue // pip only installs what it just cloned, as it always has
		}
		if _, err := os.Stat(filepath.Join(e.path, "requirements.txt")); err == nil {
			reqs = append(reqs, e)
		}
	}
	if len(reqs) > 0 && py.Available() {
		files := make([]string, len(reqs))
		for i, e := range reqs {
			files[i] = filepath.Join(e.path, "requirements.txt")
		}
		st.Step("extensions:requirements", state.StepRunning, py.Tool())
		joint := py.UV && len(files) > 1
		if joint {
			if _, err := run(ctx, "", py.InstallArgv(files...)...); err != nil {
				joint = false // find out which one
			}
		}
		if !joint {
			for i, e := range reqs {
				if out, err := run(ctx, "", py.InstallArgv(files[i])...); err != nil {
					if slices.Contains(fresh, e) || py.UV {
						st.Step(e.id, state.StepFailed, tail(out))
					}
					failed[e.id] = fmt.Errorf("could not install dependencies for %s", e.name)
				}
			}
		}
		if len(failed) > 0 {
			st.Step("extensions:requirements", state.StepFailed, py.Tool())
		} else {
			st.Step("extensions:requirements", state.StepDone, fmt.Sprintf("%s, %d extensions", py.Tool(), len(files)))
		}
	}

	for _, e := range fresh {
		if failed[e.id] == nil {
			installed = true
			st.Step(e.id, state.StepDone, e.name)
			slog.Info("installed extension", "name", e.name)
		}
	}
	for _, url := range urls { // report the first failure in manifest order
		if err := failed["extension:"+url]; err != nil {
			st.Step("extensions", state.StepFailed, err.Error())
			return installed, err
		}
	}
	st.Step("extensions", state.StepDone, fmt.Sprintf("%d cloned, %d already present", len(fresh), len(present)))
	return installed, nil
}

// Python installs packages into ComfyUI's venv, with uv when the venv has it
// and pip otherwise.
type Python struct {
	Venv string
	UV   bool
	// Constraints pins what an extension may not change (torch and its
	// companions, which the image built for its CUDA version). Optional.
	Constraints string
}

func NewPython(comfyDir string) Python {
	venv := filepath.Join(comfyDir, "venv")
	p := Python{Venv: venv}
	if _, err := os.Stat(filepath.Join(venv, "bin", "uv")); err == nil {
		p.UV = true
	}
	if _, err := os.Stat(filepath.Join(venv, "constraints.txt")); err == nil {
		p.Constraints = filepath.Join(venv, "constraints.txt")
	}
	return p
}

// Available reports whether there is anything to install with.
func (p Python) Available() bool {
	if p.UV {
		return true
	}
	_, err := os.Stat(filepath.Join(p.Venv, "bin", "pip"))
	return err == nil
}

func (p Python) Tool() string {
	if p.UV {
		return "uv"
	}
	return "pip"
}

// InstallArgv is the command that installs the given requirements files.
func (p Python) InstallArgv(requirements ...string) []string {
	var argv []string
	if p.UV {
		// unsafe-best-match is pip's behaviour: an extension that adds an
		// --extra-index-url gets the best version across indexes, not the
		// first index's. Bytecode is compiled now, on every core, rather than
		// one module at a time while ComfyUI imports it.
		argv = []string{filepath.Join(p.Venv, "bin", "uv"), "pip", "install", "--quiet",
			"--python", filepath.Join(p.Venv, "bin", "python"),
			"--index-strategy", "unsafe-best-match", "--compile-bytecode"}
	} else {
		argv = []string{filepath.Join(p.Venv, "bin", "pip"), "install", "-q"}
	}
	if p.Constraints != "" {
		argv = append(argv, "-c", p.Constraints)
	}
	for _, r := range requirements {
		argv = append(argv, "-r", r)
	}
	return argv
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
