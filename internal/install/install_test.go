package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
