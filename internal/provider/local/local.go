// Package local implements a development-only execution provider that runs
// the supervisor as a local subprocess in a private temporary directory. It
// provides NO security isolation and is refused in production mode. It exists
// so the full lifecycle can be exercised without a Kubernetes cluster.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deepakppatil/astrion/internal/provider"
)

type Provider struct {
	name    string
	baseDir string
	mu      sync.Mutex
	procs   map[string]*exec.Cmd
	// MaxConcurrent bounds local capacity.
	MaxConcurrent int
}

type meta struct {
	Owner   provider.Ownership `json:"owner"`
	PID     int                `json:"pid"`
	Created time.Time          `json:"created"`
}

func New(name, baseDir string) (*Provider, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}
	return &Provider{name: name, baseDir: baseDir, procs: map[string]*exec.Cmd{}, MaxConcurrent: 4}, nil
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) Validate(context.Context) error {
	return syscall.Access(p.baseDir, 2 /* W_OK */)
}

func (p *Provider) dir(execID string) string {
	return filepath.Join(p.baseDir, filepath.Base(execID))
}

func (p *Provider) CheckCapacity(ctx context.Context, _ provider.Spec) (provider.Capacity, error) {
	owned, err := p.ListOwned(ctx, "")
	if err != nil {
		return provider.Capacity{}, err
	}
	running := 0
	for _, s := range owned {
		if s.Phase == provider.PhaseRunning || s.Phase == provider.PhasePending {
			running++
		}
	}
	if running >= p.MaxConcurrent {
		return provider.Capacity{Available: false, Reason: fmt.Sprintf("local pool busy (%d/%d)", running, p.MaxConcurrent)}, nil
	}
	return provider.Capacity{Available: true}, nil
}

func (p *Provider) Allocate(_ context.Context, spec provider.Spec) (provider.Ref, error) {
	if len(spec.Command) == 0 {
		return provider.Ref{}, errors.New("command is required")
	}
	id := spec.Owner.ExecutionID
	ref := provider.Ref{Provider: p.name, ID: id}
	d := p.dir(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := os.Stat(filepath.Join(d, "meta.json")); err == nil {
		return ref, nil // idempotent
	}
	for _, sub := range []string{"workspace", "secrets", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(d, sub), 0o700); err != nil {
			return provider.Ref{}, err
		}
	}
	for k, v := range spec.Secrets {
		if err := os.WriteFile(filepath.Join(d, "secrets", filepath.Base(k)), v, 0o600); err != nil {
			return provider.Ref{}, err
		}
	}
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = filepath.Join(d, "workspace")
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(d, "home"),
		"TMPDIR=" + filepath.Join(d, "tmp"),
		"ASTRION_SECRETS_DIR=" + filepath.Join(d, "secrets"),
		"ASTRION_WORKSPACE_DIR=" + filepath.Join(d, "workspace"),
	}
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	logf, err := os.OpenFile(filepath.Join(d, "process.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return provider.Ref{}, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return provider.Ref{}, fmt.Errorf("start: %w", err)
	}
	m := meta{Owner: spec.Owner, PID: cmd.Process.Pid, Created: time.Now().UTC()}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(d, "meta.json"), b, 0o600); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		logf.Close()
		return provider.Ref{}, err
	}
	p.procs[id] = cmd
	deadline := spec.ActiveDeadline
	go func() {
		var timer *time.Timer
		if deadline > 0 {
			timer = time.AfterFunc(deadline, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		}
		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		logf.Close()
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		_ = os.WriteFile(filepath.Join(d, "exit"), []byte(strconv.Itoa(code)), 0o600)
		p.mu.Lock()
		delete(p.procs, id)
		p.mu.Unlock()
	}()
	return ref, nil
}

func (p *Provider) status(id string) (provider.Status, error) {
	ref := provider.Ref{Provider: p.name, ID: id}
	d := p.dir(id)
	b, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return provider.Status{Ref: ref, Phase: provider.PhaseNotFound}, nil
	}
	if err != nil {
		return provider.Status{}, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return provider.Status{}, err
	}
	st := provider.Status{Ref: ref, Owner: m.Owner, Created: m.Created}
	if code, err := os.ReadFile(filepath.Join(d, "exit")); err == nil {
		if strings.TrimSpace(string(code)) == "0" {
			st.Phase = provider.PhaseSucceeded
		} else {
			st.Phase = provider.PhaseFailed
			st.Message = "exit code " + strings.TrimSpace(string(code))
		}
		return st, nil
	}
	if syscall.Kill(m.PID, 0) == nil {
		st.Phase = provider.PhaseRunning
	} else {
		st.Phase = provider.PhaseFailed
		st.Message = "process no longer exists"
	}
	return st, nil
}

func (p *Provider) Status(_ context.Context, ref provider.Ref) (provider.Status, error) {
	return p.status(ref.ID)
}

// Terminate kills the process group and removes the directory. Idempotent.
func (p *Provider) Terminate(_ context.Context, ref provider.Ref) error {
	d := p.dir(ref.ID)
	b, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(d)
	}
	if err == nil {
		var m meta
		if json.Unmarshal(b, &m) == nil && m.PID > 0 {
			if _, err := os.Stat(filepath.Join(d, "exit")); err != nil {
				_ = syscall.Kill(-m.PID, syscall.SIGTERM)
				for i := 0; i < 20; i++ {
					if syscall.Kill(m.PID, 0) != nil {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				_ = syscall.Kill(-m.PID, syscall.SIGKILL)
			}
		}
	}
	return os.RemoveAll(d)
}

func (p *Provider) ListOwned(_ context.Context, tenantID string) ([]provider.Status, error) {
	entries, err := os.ReadDir(p.baseDir)
	if err != nil {
		return nil, err
	}
	var out []provider.Status
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := p.status(e.Name())
		if err != nil || st.Phase == provider.PhaseNotFound {
			continue
		}
		if tenantID != "" && st.Owner.TenantID != tenantID {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

var _ provider.Provider = (*Provider)(nil)
