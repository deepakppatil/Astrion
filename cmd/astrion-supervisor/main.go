// Command astrion-supervisor runs inside an execution environment. It reads
// its execution spec from the control plane, prepares repositories, runs the
// agent CLI through its adapter, streams logs, relays interactions, uploads
// artifacts and reports completion.
//
// Environment:
//
//	ASTRION_API_URL        control-plane base URL
//	ASTRION_EXECUTION_ID   execution ID
//	ASTRION_SECRETS_DIR    directory holding the "execution-token" file
//	ASTRION_WORKSPACE_DIR  scratch workspace (default /workspace)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/agent/mock"
	"github.com/deepakppatil/astrion/internal/execapi"
	"github.com/deepakppatil/astrion/internal/supervisor"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "astrion-supervisor")
	apiURL, execID := os.Getenv("ASTRION_API_URL"), os.Getenv("ASTRION_EXECUTION_ID")
	secrets := os.Getenv("ASTRION_SECRETS_DIR")
	ws := os.Getenv("ASTRION_WORKSPACE_DIR")
	if ws == "" {
		ws = "/workspace"
	}
	if apiURL == "" || execID == "" || secrets == "" {
		log.Error("ASTRION_API_URL, ASTRION_EXECUTION_ID and ASTRION_SECRETS_DIR are required")
		os.Exit(2)
	}
	tok, err := os.ReadFile(filepath.Join(secrets, "execution-token"))
	if err != nil {
		log.Error("read execution token", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := &execapi.Client{BaseURL: strings.TrimRight(apiURL, "/"), ExecutionID: execID,
		Token: strings.TrimSpace(string(tok)), HTTP: &http.Client{Timeout: 2 * time.Minute}}
	sup := supervisor.New(supervisor.Config{API: client, Adapters: agent.NewRegistry(mock.New()),
		WorkspaceDir: ws, SecretsDir: secrets, Log: log})
	if err := sup.Run(ctx); err != nil {
		if errors.Is(err, execapi.ErrGone) {
			log.Info("execution no longer active; exiting")
			return
		}
		log.Error("supervisor failed", "err", err)
		os.Exit(1)
	}
}
