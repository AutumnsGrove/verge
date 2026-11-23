// Package config turns a command line and an environment into the configuration a
// running verge needs.
//
// It is a pure function over arguments and a `getenv` function, which is what
// docs/testing.md asks for: "flags are parsed by a pure function — `Parse([]string)
// (Config, error)` — and tested before the command exists". Nothing here reads
// `os.Args`, touches a file, or dials the network, so every rule below is a table-driven
// test rather than something discovered by running a binary.
//
// Two conventions are decisions, not style:
//
//   - **Secrets come from the environment and nowhere else.** A flag is visible in `ps`
//     to every user on the box and lands in shell history; these three are a provider
//     credential that can spend money, a tailnet credential that can delete devices and
//     mint keys, and an object-storage credential that can destroy the state document.
//   - **Everything else is a flag with a default.** Durations and sizes are the sort of
//     thing an operator changes once and forgets, and a flag makes the value it ran with
//     visible in the process table — which is exactly what you want for values that are
//     not secret.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AutumnsGrove/verge/internal/net/tailnet"
)

// Environment variable names for the three secrets.
const (
	EnvHetznerToken = "VERGE_HETZNER_TOKEN"
	EnvTailnetToken = "VERGE_TAILNET_TOKEN"
	EnvS3AccessKey  = "VERGE_S3_ACCESS_KEY"
	EnvS3SecretKey  = "VERGE_S3_SECRET_KEY"
)

// Defaults. They are guesses unless the comment says otherwise, and each one is here
// rather than in `main` so a test can assert it.
const (
	DefaultInterval     = 30 * time.Second
	DefaultDrainWindow  = 15 * time.Minute
	DefaultMaxLifetime  = 4 * time.Hour
	DefaultBootDeadline = 10 * time.Minute
	DefaultTerminate    = 5 * time.Minute
	DefaultRetryBase    = 30 * time.Second
	DefaultRetryMax     = 5 * time.Minute
	DefaultSyncAttempts = 3
	DefaultKeyTTL       = 15 * time.Minute
	DefaultLabel        = "verge=machine"
	DefaultImage        = "ubuntu-24.04"
	DefaultRegion       = "fsn1"
	DefaultSize         = "cx22"
)

// Config is everything the process needs to reconcile.
//
// It holds secrets, so it must not be logged whole: `String` is redacted, and the
// `Secret` type is what makes accidentally printing one a deliberate act rather than an
// oversight.
type Config struct {
	// --- credentials (environment only) ---

	HetznerToken Secret
	TailnetToken Secret
	S3AccessKey  Secret
	S3SecretKey  Secret

	// --- where things are ---

	TailnetName string
	R2Endpoint  string
	R2Bucket    string
	R2Key       string

	// Label identifies this deployment's machines in the provider, and prefixes the
	// names firefly mints.
	Label string

	// --- what a machine is ---

	Image  string
	Region string
	Size   string

	// KeyTTL is how long an enrolment key stays usable between mint and join.
	KeyTTL time.Duration

	// --- how the loop behaves ---

	Interval          time.Duration
	DrainWindow       time.Duration
	MaxLifetime       time.Duration
	BootDeadline      time.Duration
	TerminateDeadline time.Duration
	RetryBase         time.Duration
	RetryMax          time.Duration
	SyncAttempts      int
}

// Secret is a string that does not print itself.
//
// It implements every way a value escapes into a log line, which is more than `fmt`:
//
//   - String and GoString cover `%v`, `%s`, `%+v` and `%#v` — the last being the verb a
//     debugging session reaches for, and the one a naive wrapper forgets.
//   - MarshalJSON covers a `slog` JSON handler, an HTTP debug endpoint, and anything else
//     that encodes a struct field by field. Without it, `json.Marshal(cfg)` and
//     `slog.NewJSONHandler(os.Stdout, nil).Info("cfg", "cfg", cfg)` both print all four
//     credentials — which is how a service actually logs its configuration.
//   - MarshalText covers the encoders that ask for text rather than JSON.
//
// The value needs `.Reveal()` to be read at all, which is the point: the one place a
// credential is meant to be read is the line that builds a client, and a test asserts
// nothing else prints it.
type Secret string

// Redacted is what a Secret prints as, and how long it was is not part of it.
const Redacted = "[redacted]"

// Reveal returns the secret itself.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer, so `%v`, `%s` and a struct printed with `%+v` are all
// redacted.
func (s Secret) String() string { return Redacted }

// GoString implements fmt.GoStringer, so `%#v` is redacted too — that is the verb a
// debugging session reaches for and the one a naive Secret type forgets.
func (s Secret) GoString() string { return Redacted }

// MarshalJSON implements json.Marshaler, so a structured logger or an encoder cannot
// serialize the value it is protecting.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// MarshalText implements encoding.TextMarshaler, for the encoders that ask for text.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// ErrConfig means the arguments and environment cannot describe a working process.
var ErrConfig = errors.New("config: invalid configuration")

// Parse reads args as flags and getenv for the secrets.
//
// args excludes the program name, as `flag` expects. A caller that wants `-h` handled by
// help rather than by an error passes a usage writer via WithUsage; `ErrHelp` is returned
// after the usage text is written, which is the same contract `flag` has.
func Parse(args []string, getenv func(string) string, opts ...Option) (*Config, error) {
	cfg := &Config{}
	o := options{out: io.Discard}
	for _, opt := range opts {
		opt(&o)
	}

	fs := flag.NewFlagSet("verge", flag.ContinueOnError)
	fs.SetOutput(o.out)
	fs.Usage = func() {
		fmt.Fprint(o.out, usage)
		fs.PrintDefaults()
	}

	fs.StringVar(&cfg.TailnetName, "tailnet", "-", "tailnet to join, or - for the token's default")
	fs.StringVar(&cfg.R2Endpoint, "r2-endpoint", "", "S3 endpoint URL for the state document's bucket")
	fs.StringVar(&cfg.R2Bucket, "r2-bucket", "", "bucket holding the state document (required)")
	fs.StringVar(&cfg.R2Key, "r2-key", "state.json", "object key of the state document")
	fs.StringVar(&cfg.Label, "label", DefaultLabel, "label identifying this deployment's machines, as key=value")
	fs.StringVar(&cfg.Image, "image", DefaultImage, "image to provision")
	fs.StringVar(&cfg.Region, "region", DefaultRegion, "location to provision in")
	fs.StringVar(&cfg.Size, "size", DefaultSize, "server type to provision")
	fs.DurationVar(&cfg.KeyTTL, "key-ttl", DefaultKeyTTL, "how long an enrolment key stays usable")
	fs.DurationVar(&cfg.Interval, "interval", DefaultInterval, "how often to reconcile")
	fs.DurationVar(&cfg.DrainWindow, "drain-window", DefaultDrainWindow, "how long a machine lingers after its last lease")
	fs.DurationVar(&cfg.MaxLifetime, "max-lifetime", DefaultMaxLifetime, "hard cap on a machine's life")
	fs.DurationVar(&cfg.BootDeadline, "boot-deadline", DefaultBootDeadline, "how long a boot may take before it is an error")
	fs.DurationVar(&cfg.TerminateDeadline, "terminate-deadline", DefaultTerminate, "how long a delete may stay unconfirmed")
	fs.DurationVar(&cfg.RetryBase, "retry-base", DefaultRetryBase, "backoff for the first consecutive failure")
	fs.DurationVar(&cfg.RetryMax, "retry-max", DefaultRetryMax, "ceiling on the backoff")
	fs.IntVar(&cfg.SyncAttempts, "sync-attempts", DefaultSyncAttempts, "how many times a save is retried before the cap may abandon it")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: unexpected argument %q", ErrConfig, fs.Arg(0))
	}

	cfg.HetznerToken = Secret(getenv(EnvHetznerToken))
	cfg.TailnetToken = Secret(getenv(EnvTailnetToken))
	cfg.S3AccessKey = Secret(getenv(EnvS3AccessKey))
	cfg.S3SecretKey = Secret(getenv(EnvS3SecretKey))

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Option configures Parse.
type Option func(*options)

type options struct {
	out io.Writer
}

// WithUsage sends flag errors and the usage text to w. Parse discards both by default, so
// a test does not have to capture output to assert on behaviour.
func WithUsage(w io.Writer) Option { return func(o *options) { o.out = w } }

// usage is the paragraph before the flag list.
const usage = `verge keeps exactly as many machines alive as the work needs.

Secrets come from the environment, never from a flag:
  VERGE_HETZNER_TOKEN   provider credential, can spend money
  VERGE_TAILNET_TOKEN   tailnet credential, can manage devices and mint keys
  VERGE_S3_ACCESS_KEY   object storage, can destroy the state document
  VERGE_S3_SECRET_KEY   object storage, can destroy the state document

Flags:
`

// validate reports the first thing that would make a running process wrong.
//
// Everything here is checked before the loop starts rather than discovered on the first
// tick, for the same reason firefly.New validates its Config: a bad drain window is a
// machine that fades immediately, and nobody notices until a bill.
func (c *Config) validate() error {
	var problems []string
	require := func(name string, set bool) {
		if !set {
			problems = append(problems, name+" is required")
		}
	}
	positive := func(name string, d time.Duration) {
		if d <= 0 {
			problems = append(problems, name+" must be positive")
		}
	}

	require("a Hetzner token in "+EnvHetznerToken, c.HetznerToken != "")
	require("a tailnet token in "+EnvTailnetToken, c.TailnetToken != "")
	require("an S3 access key in "+EnvS3AccessKey, c.S3AccessKey != "")
	require("an S3 secret key in "+EnvS3SecretKey, c.S3SecretKey != "")
	require("--r2-bucket", c.R2Bucket != "")
	require("--r2-endpoint", c.R2Endpoint != "")

	require("--label", c.Label != "")
	require("--image", c.Image != "")
	require("--region", c.Region != "")
	require("--size", c.Size != "")

	if c.Label != "" {
		key, value, ok := strings.Cut(c.Label, "=")
		if !ok || key == "" || value == "" {
			problems = append(problems, fmt.Sprintf("--label must be key=value, got %q", c.Label))
		}
	}

	positive("--interval", c.Interval)
	positive("--max-lifetime", c.MaxLifetime)
	positive("--boot-deadline", c.BootDeadline)
	positive("--terminate-deadline", c.TerminateDeadline)
	positive("--retry-base", c.RetryBase)
	positive("--key-ttl", c.KeyTTL)
	if c.DrainWindow < 0 {
		problems = append(problems, "--drain-window cannot be negative")
	}
	if c.RetryMax < c.RetryBase {
		problems = append(problems, "--retry-max must be at least --retry-base")
	}
	if c.SyncAttempts < 1 {
		problems = append(problems, "--sync-attempts must be at least 1")
	}
	if c.KeyTTL > tailnet.MaxKeyTTL {
		// The key minter refuses this, so without the check the operator would learn it
		// on the first provision instead of at startup.
		problems = append(problems, fmt.Sprintf(
			"--key-ttl (%s) is longer than the %s ceiling enforced when a key is minted",
			c.KeyTTL, tailnet.MaxKeyTTL))
	}
	if c.KeyTTL > 0 && c.BootDeadline > 0 && c.KeyTTL < c.BootDeadline {
		problems = append(problems, fmt.Sprintf(
			"--key-ttl (%s) is shorter than --boot-deadline (%s), so a slow boot could outlive the key that enrols it",
			c.KeyTTL, c.BootDeadline))
	}

	switch {
	case len(problems) == 0:
		return nil
	case len(problems) == 1:
		return fmt.Errorf("%w: %s", ErrConfig, problems[0])
	default:
		return fmt.Errorf("%w:\n  %s", ErrConfig, strings.Join(problems, "\n  "))
	}
}

// String renders the configuration for a log line, with every secret redacted.
//
// It exists so that the obvious `log.Printf("%+v", cfg)` cannot print a credential: this
// method is what that call uses, and `Secret` redacts even when the struct is printed
// field by field.
func (c Config) String() string {
	return fmt.Sprintf(
		"verge: tailnet=%s bucket=%s key=%s label=%q image=%s region=%s size=%s "+
			"interval=%s drain=%s max-lifetime=%s boot-deadline=%s terminate-deadline=%s "+
			"retry=%s..%s sync-attempts=%d key-ttl=%s",
		c.TailnetName, c.R2Bucket, c.R2Key, c.Label, c.Image, c.Region, c.Size,
		c.Interval, c.DrainWindow, c.MaxLifetime, c.BootDeadline, c.TerminateDeadline,
		c.RetryBase, c.RetryMax, c.SyncAttempts, c.KeyTTL)
}
