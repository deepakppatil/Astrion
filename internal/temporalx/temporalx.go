// Package temporalx holds small Temporal client helpers shared by the API
// and the worker.
package temporalx

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/durationpb"
)

// slogAdapter routes Temporal SDK logs through slog.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debug(msg string, kv ...any) { a.l.Debug(msg, kv...) }
func (a slogAdapter) Info(msg string, kv ...any)  { a.l.Info(msg, kv...) }
func (a slogAdapter) Warn(msg string, kv ...any)  { a.l.Warn(msg, kv...) }
func (a slogAdapter) Error(msg string, kv ...any) { a.l.Error(msg, kv...) }

func Options(hostPort, namespace string, log *slog.Logger) client.Options {
	return client.Options{HostPort: hostPort, Namespace: namespace, Logger: slogAdapter{log.With("component", "temporal")}}
}

// EnsureNamespace registers the namespace if missing (development
// convenience; production namespaces are provisioned by operators).
func EnsureNamespace(ctx context.Context, opts client.Options, retention time.Duration) error {
	nc, err := client.NewNamespaceClient(opts)
	if err != nil {
		return err
	}
	defer nc.Close()
	err = nc.Register(ctx, &workflowservice.RegisterNamespaceRequest{
		Namespace:                        opts.Namespace,
		WorkflowExecutionRetentionPeriod: durationpb.New(retention),
	})
	var exists *serviceerror.NamespaceAlreadyExists
	if err != nil && !errors.As(err, &exists) {
		return err
	}
	return nil
}

// Dial connects with retries so the worker tolerates Temporal starting
// after it (common with docker-compose and Kubernetes).
func Dial(ctx context.Context, opts client.Options, log *slog.Logger) (client.Client, error) {
	delay := time.Second
	for {
		c, err := client.DialContext(ctx, opts)
		if err == nil {
			return c, nil
		}
		log.Warn("temporal not reachable; retrying", "address", opts.HostPort, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay *= 2
		}
	}
}
