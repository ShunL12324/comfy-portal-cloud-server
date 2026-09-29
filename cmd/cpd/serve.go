package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/api"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/aria2"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/config"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/downloads"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/install"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/logs"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/manifest"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/pipeline"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/procs"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/redact"
	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return err
	}

	redactor := redact.New(cfg.Secrets()...)
	var out io.Writer = os.Stdout
	if f, err := os.OpenFile(cfg.LogPath("supervisor"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		defer func() { _ = f.Close() }()
		out = io.MultiWriter(os.Stdout, f)
	}
	logs.Setup(redactor.Writer(out))
	slog.Info("starting", "version", version, "listen", cfg.Listen)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	st := state.New(cfg.StatePath(), redactor)
	supervisor := procs.New(st)
	aria := aria2.New(cfg.Aria2Port, cfg.Aria2Secret)
	dl := downloads.New(downloads.Config{
		Workspace: cfg.Workspace, HFToken: cfg.HFToken, CivitaiAPIKey: cfg.CivitaiAPIKey,
		StallAfter: cfg.StallAfter, PollInterval: cfg.PollInterval,
	}, st, aria, redactor)
	pipe := &pipeline.Pipeline{Cfg: cfg, St: st, Procs: supervisor, DL: dl, Aria: aria, Run: install.Exec}

	handler := api.New(api.Options{
		State: st, Controller: &controller{ctx: ctx, pipe: pipe, st: st}, Redactor: redactor,
		Token: cfg.Token, Version: version, LogPath: cfg.LogPath,
	}).Handler()
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: /events streams for as long as the client stays.
	}
	// Listen before anything slow, so /health answers within a second of the
	// container starting instead of after the install.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.Serve(ln) }()

	go func() {
		// The HTTP server has to outlive a failed launch: a dead supervisor with
		// a reason is far more useful than an unreachable port.
		defer func() {
			if v := recover(); v != nil {
				st.Fail("supervisor_crashed", fmt.Sprint(v), "")
			}
		}()
		if err := install.StartSSH(ctx, install.Exec, cfg.SSHPublicKey); err != nil {
			st.Fail("ssh_failed", err.Error(), "")
			return
		}
		m, err := manifest.Decode(cfg.ManifestB64)
		if err != nil {
			st.Fail("bad_manifest", err.Error(), "")
			return
		}
		pipe.Boot(ctx, m)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-httpErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	pipe.WaitBackground()
	supervisor.Wait() // stops ComfyUI, Ollama and aria2 before exiting
	st.Persist()
	return nil
}

// controller adapts the pipeline to the API's Controller interface.
type controller struct {
	// ctx outlives any request: applying a manifest keeps installing after the
	// response has been sent.
	ctx  context.Context
	pipe *pipeline.Pipeline
	st   *state.State
}

func (c *controller) RetryModels(ctx context.Context, ids []string) []string {
	return c.pipe.DL.Retry(ctx, ids)
}

func (c *controller) ApplyManifest(_ context.Context, m manifest.Manifest) error {
	return c.pipe.Apply(c.ctx, m)
}

func (c *controller) Manifest() manifest.Manifest { return c.pipe.Manifest() }

func (c *controller) RestartService(name string) error {
	if !c.pipe.Procs.Has(name) {
		return fmt.Errorf("%s has not started yet", name)
	}
	if err := c.pipe.Procs.Restart(name); err != nil {
		return err
	}
	if name == "comfyui" {
		c.st.SetRestartRequired(false)
	}
	return nil
}
