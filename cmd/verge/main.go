// Command verge is the control plane: it owns machines, keeps them alive while
// work holds leases on them, and destroys them after their state is saved.
//
// It is deliberately thin. Every decision lives in a package that can be tested
// without a cloud -- firefly owns when a machine exists, loop owns the clock,
// config owns parsing -- so what is left here is the joining of them, which is the
// part no test can do for you anyway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/AutumnsGrove/verge/internal/cloud/hetzner"
	"github.com/AutumnsGrove/verge/internal/config"
	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/loop"
	"github.com/AutumnsGrove/verge/internal/net/tailnet"
	"github.com/AutumnsGrove/verge/internal/store/r2"
	"github.com/AutumnsGrove/verge/internal/syncer"
)

// workspacePrefix is where this deployment's workspace tarballs live inside the
// state bucket. It is derived from the label rather than configured, so two
// deployments that already cannot see each other's machines also cannot overwrite
// each other's workspaces.
func workspacePrefix(label string) string { return "workspaces/" + label }

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The usage text has already been written; -h is not a failure.
			return
		}
		fmt.Fprintln(os.Stderr, "verge:", err)
		os.Exit(1)
	}
}

// run parses the arguments, builds the clients, and reconciles until interrupted.
//
// The context is the process's: SIGINT and SIGTERM cancel it, the pass in flight
// finishes, and run returns. Nothing here calls os.Exit, so the deferred cleanup
// in this function -- and in whatever it calls -- actually runs.
func run(args []string, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.Parse(args, getenv, config.WithUsage(stderr))
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	releaseOnCancel(ctx, stop)

	f, err := build(cfg)
	if err != nil {
		return err
	}

	// The binary can reconcile but not yet save: the machine-side agent that
	// streams a workspace over the tailnet does not exist. Say so once, loudly,
	// rather than discovering it as an abandoned workspace at the cap -- which is
	// what the fetcher below would produce, and why it is an error rather than an
	// empty stream that would look like "nothing to save".
	log.Warn("workspace saves will fail: the machine-side transport is not implemented; machines will not be destroyed by this binary until it is")

	l, err := loop.New(loop.Config{Tick: f.Tick, Interval: cfg.Interval, Log: log})
	if err != nil {
		return err
	}
	return l.Run(ctx)
}

// releaseOnCancel gives the default signal action back as soon as ctx is done.
//
// The first signal asks the loop to stop, and it is deliberately not allowed to
// interrupt the pass in flight. That makes a second signal the operator's exit from
// a pass that is stuck, so the handler is released the moment the first one lands:
// a swallowed Ctrl-C on a hung daemon is how someone reaches for SIGKILL and loses
// the tick anyway. stop is idempotent, so the deferred call in run is free.
func releaseOnCancel(ctx context.Context, stop func()) {
	go func() {
		<-ctx.Done()
		stop()
	}()
}

// build joins the four boundaries firefly needs onto one configuration.
//
// Nothing here dials: the clients are constructed and validated, and the first
// provider call happens on the first tick, which is the same rule firefly.New
// follows. A process that refuses to start because a bucket is momentarily
// unreachable is worse than one that starts and reports the failure per pass.
func build(cfg *config.Config) (*firefly.Firefly, error) {
	api := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey.Reveal(), cfg.S3SecretKey.Reveal(), ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.R2Endpoint)
		// R2 speaks S3 with path-style addressing, and a bucket in the host
		// position would make the certificate the endpoint's problem rather than
		// the request's.
		o.UsePathStyle = true
	})

	store, err := r2.New(r2.Config{Bucket: cfg.R2Bucket, Key: cfg.R2Key, API: api})
	if err != nil {
		return nil, err
	}
	workspaces, err := r2.NewWorkspace(r2.WorkspaceConfig{Bucket: cfg.R2Bucket, API: api})
	if err != nil {
		return nil, err
	}

	saves, err := syncer.New(syncer.Config{
		Fetcher: noTransport{},
		Sink:    workspaces,
		Prefix:  workspacePrefix(cfg.Label),
	})
	if err != nil {
		return nil, err
	}

	return firefly.New(firefly.Config{
		Store:             store,
		Cloud:             hetzner.New(cfg.HetznerToken.Reveal()),
		Network:           tailnet.New(cfg.TailnetToken.Reveal(), tailnet.WithTailnet(cfg.TailnetName)),
		Syncer:            saves,
		Label:             cfg.Label,
		MaxLifetime:       cfg.MaxLifetime,
		DrainWindow:       cfg.DrainWindow,
		BootDeadline:      cfg.BootDeadline,
		SyncAttempts:      cfg.SyncAttempts,
		TerminateDeadline: cfg.TerminateDeadline,
		RetryBase:         cfg.RetryBase,
		RetryMax:          cfg.RetryMax,
	})
}

// noTransport is the fetcher that has no machine to fetch from.
//
// It returns an error rather than an empty stream, and the distinction is the whole
// point of the syncer: an empty stream means the machine had nothing to save, and
// firefly would treat that as a successful save and destroy the machine. An error
// means "we could not ask", which is the case in which the workspace -- and the
// person's afternoon -- is kept.
type noTransport struct{}

// ErrNoTransport means the workspace transport does not exist yet.
var ErrNoTransport = errors.New("verge: the machine-side workspace transport is not implemented")

func (noTransport) Fetch(ctx context.Context, id firefly.MachineID, name string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: cannot fetch the workspace of %s (%s)", ErrNoTransport, id, name)
}
