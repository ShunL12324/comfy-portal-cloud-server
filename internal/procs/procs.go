// Package procs keeps long-running child processes alive.
//
// Staying resident past "ready" is the whole reason a server can go offline
// and the user can still find out why: a crashed ComfyUI otherwise looks
// identical to a network problem from the app's side.
package procs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ShunL12324/comfy-portal-cloud-server/internal/state"
)

type Spec struct {
	Name    string
	Argv    []string
	Dir     string
	Env     []string // appended to the parent's environment
	LogPath string
	// ReadyURL, when set, is polled after every start; the service is
	// "starting" until it answers 200 and only then "running".
	ReadyURL string
}

type Supervisor struct {
	st *state.State
	// Backoff bounds for restarting a process that keeps dying.
	MinBackoff, MaxBackoff time.Duration
	// A process that stayed up this long is considered healthy and its backoff resets.
	StableAfter time.Duration
	StopTimeout time.Duration

	mu    sync.Mutex
	procs map[string]*proc
	wg    sync.WaitGroup
}

type proc struct {
	spec    Spec
	restart chan struct{}
}

func New(st *state.State) *Supervisor {
	return &Supervisor{
		st: st, MinBackoff: time.Second, MaxBackoff: 30 * time.Second,
		StableAfter: time.Minute, StopTimeout: 10 * time.Second,
		procs: map[string]*proc{},
	}
}

// Start begins supervising spec until ctx is cancelled.
func (s *Supervisor) Start(ctx context.Context, spec Spec) error {
	s.mu.Lock()
	if _, ok := s.procs[spec.Name]; ok {
		s.mu.Unlock()
		return fmt.Errorf("%s is already supervised", spec.Name)
	}
	p := &proc{spec: spec, restart: make(chan struct{}, 1)}
	s.procs[spec.Name] = p
	s.mu.Unlock()

	s.st.UpdateService(spec.Name, func(v *state.Service) { v.State = state.ServiceStarting })
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx, p)
	}()
	return nil
}

// Restart asks the named service to be stopped and started again.
func (s *Supervisor) Restart(name string) error {
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such service %q", name)
	}
	select {
	case p.restart <- struct{}{}:
	default: // one is already pending
	}
	return nil
}

// Has reports whether a service is supervised.
func (s *Supervisor) Has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.procs[name]
	return ok
}

// Wait blocks until every supervised process has been stopped. Cancel the
// context passed to Start first.
func (s *Supervisor) Wait() { s.wg.Wait() }

// WaitRunning blocks until the service answers its ready URL.
func (s *Supervisor) WaitRunning(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		if svc, ok := s.st.Service(name); ok && svc.State == state.ServiceRunning {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%s did not answer within %s", name, timeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Supervisor) loop(ctx context.Context, p *proc) {
	backoff := s.MinBackoff
	for {
		if ctx.Err() != nil {
			s.st.UpdateService(p.spec.Name, func(v *state.Service) { v.State, v.PID = state.ServiceStopped, 0 })
			return
		}
		startedAt := time.Now()
		cmd, logFile, err := s.spawn(p.spec)
		if err != nil {
			slog.Error("could not start", "service", p.spec.Name, "err", err)
			s.st.UpdateService(p.spec.Name, func(v *state.Service) {
				v.State = state.ServiceRestarting
				v.Restarts++
			})
		} else {
			s.st.UpdateService(p.spec.Name, func(v *state.Service) {
				v.State, v.PID = state.ServiceStarting, cmd.Process.Pid
				if p.spec.ReadyURL == "" {
					v.State = state.ServiceRunning
				}
			})
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			probeCtx, stopProbe := context.WithCancel(ctx)
			if p.spec.ReadyURL != "" {
				go s.probe(probeCtx, p.spec)
			}

			manual := false
			select {
			case err = <-done:
			case <-p.restart:
				manual = true
				s.terminate(cmd, done)
			case <-ctx.Done():
				s.terminate(cmd, done)
				stopProbe()
				_ = logFile.Close()
				s.st.UpdateService(p.spec.Name, func(v *state.Service) { v.State, v.PID = state.ServiceStopped, 0 })
				return
			}
			stopProbe()
			_ = logFile.Close()
			code := cmd.ProcessState.ExitCode()
			slog.Warn("exited", "service", p.spec.Name, "code", code, "manual", manual)
			s.st.UpdateService(p.spec.Name, func(v *state.Service) {
				v.State, v.PID = state.ServiceRestarting, 0
				v.Restarts++
				v.LastExit = &code
			})
			if manual {
				backoff = s.MinBackoff
				continue // restarted on request: no delay
			}
			if time.Since(startedAt) > s.StableAfter {
				backoff = s.MinBackoff
			}
		}
		select {
		case <-ctx.Done():
		case <-p.restart:
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.MaxBackoff)
	}
}

func (s *Supervisor) spawn(spec Spec) (*exec.Cmd, *os.File, error) {
	logFile, err := os.OpenFile(spec.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Own process group, so stopping a service takes its children with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, nil, err
	}
	return cmd, logFile, nil
}

// terminate asks the process group to exit, then insists.
func (s *Supervisor) terminate(cmd *exec.Cmd, done <-chan error) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(s.StopTimeout):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-done
	}
}

// probe polls the ready URL until it answers, then marks the service running.
// Custom nodes import at startup and some are slow, so this can take minutes;
// the service stays "starting" meanwhile rather than claiming to be up.
func (s *Supervisor) probe(ctx context.Context, spec Spec) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, spec.ReadyURL, nil)
		if resp, err := client.Do(req); err == nil {
			ok := resp.StatusCode == http.StatusOK
			_ = resp.Body.Close()
			if ok {
				s.st.UpdateService(spec.Name, func(v *state.Service) {
					v.State, v.AnsweredAt = state.ServiceRunning, time.Now().Unix()
				})
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
