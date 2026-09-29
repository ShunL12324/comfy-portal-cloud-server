package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

func TestLinkIntoComfyMovesExistingCustomNodes(t *testing.T) {
	ws, comfy := t.TempDir(), t.TempDir()
	nodes := filepath.Join(comfy, "custom_nodes", "ComfyUI-Manager")
	_ = os.MkdirAll(nodes, 0o755)
	_ = os.WriteFile(filepath.Join(nodes, "x.py"), []byte("1"), 0o644)
	_ = os.MkdirAll(filepath.Join(comfy, "models"), 0o755)

	for i := 0; i < 2; i++ { // twice: must be idempotent
		if err := LinkIntoComfy(ws, comfy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "custom_nodes", "ComfyUI-Manager", "x.py")); err != nil {
		t.Fatal("image's custom node was lost:", err)
	}
	if target, err := os.Readlink(filepath.Join(comfy, "custom_nodes")); err != nil || target != filepath.Join(ws, "custom_nodes") {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if _, err := os.Stat(filepath.Join(comfy, "output")); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionsCloneInstallAndSkip(t *testing.T) {
	ws, comfy := t.TempDir(), t.TempDir()
	st := state.New("", redact.New())
	var calls []string
	run := func(_ context.Context, _ string, argv ...string) (string, error) {
		calls = append(calls, strings.Join(argv[:2], " "))
		if argv[0] == "git" {
			dest := argv[len(argv)-1]
			_ = os.MkdirAll(dest, 0o755)
			_ = os.WriteFile(filepath.Join(dest, "requirements.txt"), []byte("x"), 0o644)
		}
		return "", nil
	}
	_ = os.MkdirAll(filepath.Join(comfy, "venv", "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(comfy, "venv", "bin", "pip"), nil, 0o755)

	urls := []string{"https://github.com/u/Ext-A.git"}
	installed, err := Extensions(context.Background(), st, run, ws, comfy, urls)
	if err != nil || !installed || len(calls) != 2 {
		t.Fatalf("installed=%v err=%v calls=%v", installed, err, calls)
	}
	installed, err = Extensions(context.Background(), st, run, ws, comfy, urls)
	if err != nil || installed || len(calls) != 2 {
		t.Fatalf("second pass must be a no-op: installed=%v calls=%v", installed, calls)
	}
}

func TestFailedCloneLeavesNothingBehind(t *testing.T) {
	ws := t.TempDir()
	st := state.New("", redact.New())
	run := func(_ context.Context, _ string, argv ...string) (string, error) {
		_ = os.MkdirAll(argv[len(argv)-1], 0o755)
		return "fatal: repository not found", errors.New("exit 128")
	}
	_, err := Extensions(context.Background(), st, run, ws, t.TempDir(), []string{"https://github.com/u/gone"})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(filepath.Join(ws, "custom_nodes", "gone")); statErr == nil {
		t.Fatal("half-clone left on disk")
	}
	if s := st.Steps()[0]; s.State != state.StepFailed || !strings.Contains(s.Detail, "not found") {
		t.Fatalf("%+v", s)
	}
}

func TestValidateSSHKey(t *testing.T) {
	good := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc user@host"
	if err := ValidateSSHKey(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "ssh-dss AAAA x", "ssh-ed25519", "ssh-ed25519 !!!notb64", good + "\nssh-rsa AAAA x"} {
		if ValidateSSHKey(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// crossDevice makes every rename fail the way it does when /workspace is a
// different mount from the image layer.
func crossDevice(t *testing.T) {
	t.Helper()
	orig := renameFn
	renameFn = func(oldpath, newpath string) error {
		if strings.HasSuffix(newpath, ".partial") || strings.Contains(newpath, "/.partial") {
			return orig(oldpath, newpath)
		}
		if strings.HasSuffix(oldpath, ".partial") {
			return orig(oldpath, newpath) // the final in-place rename is same-device
		}
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFn = orig })
}

func TestLinkIntoComfyAcrossDevices(t *testing.T) {
	crossDevice(t)
	ws, comfy := t.TempDir(), t.TempDir()
	node := filepath.Join(comfy, "custom_nodes", "ComfyUI-Manager")
	_ = os.MkdirAll(filepath.Join(node, "sub", "deep"), 0o755)
	_ = os.WriteFile(filepath.Join(node, "sub", "deep", "x.py"), []byte("payload"), 0o640)
	_ = os.Symlink("sub/deep/x.py", filepath.Join(node, "link.py"))
	_ = os.WriteFile(filepath.Join(node, "run.sh"), []byte("#!/bin/sh"), 0o755)
	_ = os.MkdirAll(filepath.Join(comfy, "models", "audio_encoders"), 0o755)
	_ = os.WriteFile(filepath.Join(comfy, "models", "audio_encoders", "put_here.txt"), []byte("x"), 0o644)

	if err := LinkIntoComfy(ws, comfy); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(ws, "custom_nodes", "ComfyUI-Manager")
	got, err := os.ReadFile(filepath.Join(moved, "sub", "deep", "x.py"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("file lost: %q %v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(moved, "link.py")); err != nil || target != "sub/deep/x.py" {
		t.Fatalf("symlink lost: %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(moved, "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode lost: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "models", "audio_encoders", "put_here.txt")); err != nil {
		t.Fatal("models subfolder lost:", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(ws, "*", "*.partial")); len(entries) != 0 {
		t.Fatalf("partial copy left behind: %v", entries)
	}
	if target, err := os.Readlink(filepath.Join(comfy, "custom_nodes")); err != nil || target != filepath.Join(ws, "custom_nodes") {
		t.Fatalf("not linked: %q %v", target, err)
	}
}

func TestInterruptedCopyIsNotTrusted(t *testing.T) {
	crossDevice(t)
	src, dstDir := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "unreadable"), []byte("2"), 0o000)
	dst := filepath.Join(dstDir, "moved")
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	if err := move(src, dst); err == nil {
		t.Fatal("expected the copy to fail")
	}
	if _, err := os.Lstat(dst); err == nil {
		t.Fatal("a failed copy must not leave dst behind, or a retry would skip it")
	}
	if _, err := os.Lstat(dst + ".partial"); err == nil {
		t.Fatal("partial not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(src, "a")); err != nil {
		t.Fatal("source must survive a failed move")
	}
}
