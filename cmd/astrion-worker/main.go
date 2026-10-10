// Command astrion-worker runs Temporal job workflows and activities, the
// workflow dispatcher (outbox for unstarted jobs) and the orphan reconciler.
package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.temporal.io/sdk/worker"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/agent/mock"
	"github.com/deepakppatil/astrion/internal/config"
	"github.com/deepakppatil/astrion/internal/orchestrator"
	"github.com/deepakppatil/astrion/internal/reconciler"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/temporalx"
	"github.com/deepakppatil/astrion/internal/workflow"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		cfg.Logger().Error("fatal", "service", "astrion-worker", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	log := cfg.Logger().With("service", "astrion-worker")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	pools, err := cfg.Pools()
	if err != nil {
		return err
	}
	opts := temporalx.Options(cfg.TemporalAddress, cfg.TemporalNamespace, log)
	if os.Getenv("ASTRION_TEMPORAL_REGISTER_NAMESPACE") == "true" || (!cfg.Production && os.Getenv("ASTRION_TEMPORAL_REGISTER_NAMESPACE") != "false") {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := temporalx.EnsureNamespace(rctx, opts, 72*time.Hour)
		cancel()
		if err != nil {
			log.Warn("namespace registration failed (continuing)", "err", err)
		}
	}
	tc, err := temporalx.Dial(ctx, opts, log)
	if err != nil {
		return err
	}
	defer tc.Close()

	acts := &workflow.Activities{Store: st, Pools: pools, Agents: agent.NewRegistry(mock.New()), Log: log,
		Exec: workflow.ExecConfig{APIURL: cfg.ExecAPIURL, DefaultImage: cfg.DefaultImage, SupervisorCommand: cfg.SupervisorCommand,
			TokenTTLSlack: 30 * time.Minute, NetworkProfile: os.Getenv("ASTRION_EXEC_NETWORK_PROFILE"),
			ServiceAccount: os.Getenv("ASTRION_EXEC_SERVICE_ACCOUNT")}}
	if !cfg.Production {
		// Local executions resolve the mock agent CLI from PATH.
		acts.Exec.Env = map[string]string{"ASTRION_LOG_LEVEL": "info"}
	}
	w := worker.New(tc, workflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflow.JobWorkflow)
	w.RegisterActivity(acts)

	engine := &orchestrator.Temporal{Client: tc, Store: st, Log: log}
	rec := &reconciler.Reconciler{Store: st, Pools: pools, Releaser: acts, Log: log}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); orchestrator.RunDispatcher(ctx, st, engine, log, 5*time.Second) }()
	go func() { defer wg.Done(); rec.Run(ctx, time.Minute) }()

	if err := w.Start(); err != nil {
		return err
	}
	log.Info("worker started", "taskQueue", workflow.TaskQueue, "pools", len(pools.All()))
	<-ctx.Done()
	log.Info("shutting down")
	w.Stop()
	wg.Wait()
	return nil
}
