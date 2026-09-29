// Package pipeline brings the instance from "the container just started" to
// "ComfyUI is serving", and can apply further manifests afterwards.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/aria2"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/config"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/downloads"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/install"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/procs"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

// Errors Apply returns that the API maps to specific statuses.
var (
	ErrNotReady = errors.New("the instance is not ready")
	ErrBusy     = errors.New("an install is already running")
)

type Pipeline struct {
	Cfg   config.Config
	St    *state.State
	Procs *procs.Supervisor
	DL    *downloads.Manager
	Aria  *aria2.Client
	Run   install.Runner
	// StartEngine starts the download engine. Defaults to running aria2c;
	// tests replace it.
	StartEngine func(ctx context.Context) error

	mu        sync.Mutex
	current   manifest.Manifest
	installMu sync.Mutex
	// bg tracks Apply's background installs so shutdown can wait for them.
	bg sync.WaitGroup
}

// Manifest is everything asked for so far, from launch and later applies.
func (p *Pipeline) Manifest() manifest.Manifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// Boot runs the launch sequence. It returns once ComfyUI answers (or the
// launch has failed); the processes it started keep running under ctx.
func (p *Pipeline) Boot(ctx context.Context, m manifest.Manifest) {
	p.mu.Lock()
	p.current = m
	p.mu.Unlock()

	st := p.St
	st.SetPhase(state.PhasePreparing)
	st.Step("link-directories", state.StepRunning, "")
	if err := install.LinkIntoComfy(p.Cfg.Workspace, p.Cfg.ComfyDir); err != nil {
		st.Step("link-directories", state.StepFailed, err.Error())
		st.Fail("link_failed", err.Error(), "")
		return
	}
	st.Step("link-directories", state.StepDone, "")

	if len(m.Models) > 0 {
		st.Step("disk-check", state.StepRunning, "")
		if _, err := p.DL.Preflight(ctx, m.Models); err != nil {
			st.Step("disk-check", state.StepFailed, "")
			var de *downloads.DiskError
			if errors.As(err, &de) {
				st.Fail("disk_full", err.Error(), "Relaunch with a larger disk in the template.")
			} else {
				st.Fail("disk_check_failed", err.Error(), "")
			}
			return
		}
		st.Step("disk-check", state.StepDone, "")
	}

	st.Step("aria2", state.StepRunning, "")
	start := p.StartEngine
	if start == nil {
		start = p.startAria2
	}
	if err := start(ctx); err != nil {
		st.Step("aria2", state.StepFailed, "")
		st.Fail("aria2_failed", err.Error(), "")
		return
	}
	st.Step("aria2", state.StepDone, "")

	// Extensions and Ollama proceed while models download: on a fresh instance
	// the models are the clock, and everything else is free if it overlaps.
	st.SetPhase(state.PhaseDownloading)
	go p.DL.Run(ctx)
	p.DL.Queue(ctx, m.Models)

	side := make(chan error, 1)
	go func() {
		_, err := p.installEnvironment(ctx, m)
		side <- err
	}()

	failed, err := p.DL.Wait(ctx)
	if err != nil {
		return
	}
	if len(failed) > 0 {
		// Not fatal on purpose: a server with 19 of 20 models is worth far more
		// than no server, and the app offers a retry per model.
		slog.Warn("some models failed", "count", len(failed))
	}
	select {
	case err := <-side:
		if err != nil {
			st.Fail("environment_install_failed", err.Error(), "")
			return
		}
	case <-time.After(p.Cfg.InstallTimeout):
		st.Fail("environment_install_timeout", "Extensions or Ollama installation exceeded 30 minutes.", "")
		return
	case <-ctx.Done():
		return
	}

	st.SetPhase(state.PhaseStarting)
	err = p.Procs.Start(ctx, procs.Spec{
		Name:     "comfyui",
		Argv:     []string{filepath.Join(p.Cfg.ComfyDir, "venv", "bin", "python"), "main.py", "--listen", "0.0.0.0", "--port", strconv.Itoa(p.Cfg.ComfyPort)},
		Dir:      p.Cfg.ComfyDir,
		LogPath:  p.Cfg.LogPath("comfyui"),
		ReadyURL: fmt.Sprintf("http://127.0.0.1:%d/system_stats", p.Cfg.ComfyPort),
	})
	if err != nil {
		st.Fail("comfy_start_failed", err.Error(), "")
		return
	}
	st.Step("comfyui-start", state.StepRunning, "waiting for /system_stats")
	if err := p.Procs.WaitRunning(ctx, "comfyui", p.Cfg.ComfyStartTimeout); err != nil {
		if ctx.Err() != nil {
			return
		}
		st.Step("comfyui-start", state.StepFailed, "")
		st.Fail("comfy_start_timeout", "ComfyUI did not answer after 10 minutes.", "Check the comfyui log stream.")
		return
	}
	st.Step("comfyui-start", state.StepDone, "")
	st.SetPhase(state.PhaseReady)
	slog.Info("ready")
}

func (p *Pipeline) startAria2(ctx context.Context) error {
	argv := append([]string{"aria2c"}, aria2.Args(p.Cfg.Aria2Port, p.Cfg.Aria2Secret, p.Cfg.MaxDownloads)...)
	if _, err := exec.LookPath("aria2c"); err != nil {
		return errors.New("aria2c is not installed")
	}
	if err := p.Procs.Start(ctx, procs.Spec{Name: "aria2", Argv: argv, LogPath: p.Cfg.LogPath("aria2")}); err != nil {
		return err
	}
	return p.Aria.WaitReady(ctx, 10*time.Second)
}

// installEnvironment installs extensions and pulls Ollama models. It
// reports whether new extensions were installed.
func (p *Pipeline) installEnvironment(ctx context.Context, m manifest.Manifest) (bool, error) {
	p.installMu.Lock()
	defer p.installMu.Unlock()

	installed, err := install.Extensions(ctx, p.St, p.Run, p.Cfg.Workspace, p.Cfg.ComfyDir, m.Extensions)
	if err != nil {
		return installed, err
	}
	if len(m.OllamaModels) == 0 {
		return installed, nil
	}
	if _, err := exec.LookPath("ollama"); err != nil {
		slog.Warn("ollama models requested but ollama is not installed (use the -ollama image)")
		return installed, nil
	}
	if !p.Procs.Has("ollama") {
		err := p.Procs.Start(ctx, procs.Spec{
			Name:     "ollama",
			Argv:     []string{"ollama", "serve"},
			LogPath:  p.Cfg.LogPath("ollama"),
			ReadyURL: "http://127.0.0.1:11434/",
		})
		if err != nil {
			return installed, err
		}
	}
	if err := p.Procs.WaitRunning(ctx, "ollama", time.Minute); err != nil {
		return installed, err
	}
	if err := install.PullOllama(ctx, p.St, p.Run, m.OllamaModels); err != nil {
		return installed, err
	}
	p.St.UpdateService("ollama", func(s *state.Service) { s.Models = m.OllamaModels })
	return installed, nil
}

// Apply adds a manifest to a running instance: new models start downloading,
// new extensions and Ollama models install in the background. Nothing is
// removed. Extensions only take effect after ComfyUI restarts, which the
// state's restartRequired flag reports.
func (p *Pipeline) Apply(ctx context.Context, m manifest.Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if p.St.Phase() != state.PhaseReady {
		return ErrNotReady
	}
	if !p.installMu.TryLock() {
		return ErrBusy
	}
	p.installMu.Unlock()

	if len(m.Models) > 0 {
		if _, err := p.DL.Preflight(ctx, m.Models); err != nil {
			return err
		}
	}

	p.mu.Lock()
	p.current = merge(p.current, m)
	p.mu.Unlock()

	p.DL.Queue(ctx, m.Models)
	p.bg.Add(1)
	go func() {
		defer p.bg.Done()
		installed, err := p.installEnvironment(ctx, m)
		if installed {
			p.St.SetRestartRequired(true)
		}
		if err != nil {
			slog.Error("applying manifest failed", "err", err)
		}
	}()
	return nil
}

// WaitBackground blocks until in-flight applies finish.
func (p *Pipeline) WaitBackground() { p.bg.Wait() }

func merge(a, b manifest.Manifest) manifest.Manifest {
	seenModel := map[string]bool{}
	var models []manifest.Model
	for _, m := range append(append([]manifest.Model{}, a.Models...), b.Models...) {
		if id := m.Resolve().ID; !seenModel[id] {
			seenModel[id] = true
			models = append(models, m)
		}
	}
	return manifest.Manifest{
		Version:      max(a.Version, b.Version),
		Models:       models,
		Extensions:   union(a.Extensions, b.Extensions),
		OllamaModels: union(a.OllamaModels, b.OllamaModels),
	}
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
