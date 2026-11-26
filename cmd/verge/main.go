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
	"strings"
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
	"github.com/AutumnsGrove/verge/internal/syncer/ssh"
)

// workspacePrefix is where this deployment's workspace tarballs live inside the
// state bucket. It is derived from the label rather than configured, so two
// deployments that already cannot see each other's machines also cannot overwrite
// each other's workspaces.
//
// The label is "key=value", which is not an object-key element, so its punctuation is
// mapped to a dash rather than passed through: the store validates keys, and a prefix
// that made every save fail with a configuration error would be a bad way to find out
// that "=" is not allowed. Two labels that sanitise to the same element share a prefix,
// which is why the sanitising is documented here rather than guessed at.
func workspacePrefix(label string) string {
	var b strings.Builder
	b.WriteString("workspaces/")
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

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

	// Where a save goes, said once at startup because it is the one thing an
	// operator has to check against the image: the machine must provide this
	// command, or every save fails with "command not found".
	log.Info("workspace saves run over ssh as " + ssh.DefaultUser + "@<machine> " + ssh.DefaultCommand)

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

	// Tailscale SSH is the transport: it needs no listener on the machine and no
	// second credential, because the tailnet already decides who may connect. The
	// host-key policy is the deployment's, so no arguments are added here -- a
	// control plane with a known_hosts file and one without want different flags,
	// and neither belongs in this file's opinion.
	transport, err := ssh.New(ssh.Config{Binary: "ssh"})
	if err != nil {
		return nil, err
	}

	saves, err := syncer.New(syncer.Config{
		Fetcher: transport,
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
