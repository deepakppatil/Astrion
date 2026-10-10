// Command astrion-api serves the control-plane HTTP API, the internal
// execution API and (optionally) the built web UI.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/agent/mock"
	"github.com/deepakppatil/astrion/internal/api"
	"github.com/deepakppatil/astrion/internal/bootstrap"
	"github.com/deepakppatil/astrion/internal/config"
	"github.com/deepakppatil/astrion/internal/events"
	"github.com/deepakppatil/astrion/internal/orchestrator"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/temporalx"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		os.Stderr.WriteString("config: " + err.Error() + "\n")
		os.Exit(2)
	}
	log := cfg.Logger().With("service", "astrion-api")
	if err := run(cfg); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	log := cfg.Logger().With("service", "astrion-api")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if os.Getenv("ASTRION_MIGRATE") != "false" {
		if err := st.Migrate(ctx); err != nil {
			return err
		}
	}
	boot, err := bootstrap.Run(ctx, st, bootstrap.Options{TenantID: cfg.TenantID, Demo: cfg.SeedDemo, DemoRepoURL: cfg.DemoRepoURL})
	if err != nil {
		return err
	}
	log.Info("bootstrap complete", "tenant", cfg.TenantID, "framework", boot.FrameworkID, "demoWorkspace", boot.WorkspaceID)

	authn, err := cfg.Authenticator()
	if err != nil {
		return err
	}
	objs, err := cfg.ObjectStore()
	if err != nil {
		return err
	}
	pools, err := cfg.Pools()
	if err != nil {
		return err
	}
	// A lazy client lets the API accept jobs while Temporal is unavailable;
	// the worker's dispatcher starts their workflows later.
	tc, err := client.NewLazyClient(temporalx.Options(cfg.TemporalAddress, cfg.TemporalNamespace, log))
	if err != nil {
		return err
	}
	defer tc.Close()

	brokerCtx, stopBroker := context.WithCancel(context.Background())
	broker := events.NewBroker(st.Pool, store.NotifyChannel, log)
	brokerDone := make(chan struct{})
	go func() { broker.Run(brokerCtx); close(brokerDone) }()
	// The broker holds a LISTEN connection; stop it before closing the pool.
	defer func() { stopBroker(); <-brokerDone }()

	srv := &api.Server{Store: st, Engine: &orchestrator.Temporal{Client: tc, Store: st, Log: log}, Auth: authn, Objects: objs,
		Broker: broker, Agents: agent.NewRegistry(mock.New()), Pools: pools, Log: log,
		Config: api.Config{Production: cfg.Production, MaxArtifactBytes: cfg.MaxArtifactBytes, MaxSubmissionsPerMinute: 30,
			MaxQueuedPerTenant: 200, StaticDir: cfg.WebDir}}
	hs := &http.Server{Addr: cfg.HTTPAddr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "production", cfg.Production)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}
