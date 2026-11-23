package config

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"
)

// env is the environment every happy case needs: all four secrets present, nothing else.
func env(overrides ...string) func(string) string {
	m := map[string]string{
		EnvHetznerToken: "hz-token",
		EnvTailnetToken: "ts-token",
		EnvS3AccessKey:  "access",
		EnvS3SecretKey:  "secret",
	}
	for i := 0; i+1 < len(overrides); i += 2 {
		m[overrides[i]] = overrides[i+1]
	}
	return func(k string) string { return m[k] }
}

// required is the minimum an operator has to say: the bucket and the endpoint.
var required = []string{"--r2-bucket", "state", "--r2-endpoint", "https://r2.example"}

func parse(t *testing.T, getenv func(string) string, args ...string) (*Config, error) {
	t.Helper()
	return Parse(args, getenv)
}

func TestParseDefaults(t *testing.T) {
	t.Run("happy/the_defaults_are_a_running_process", func(t *testing.T) {
		cfg, err := parse(t, env(), required...)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		cases := []struct {
			name string
			got  any
			want any
		}{
			{"TailnetName", cfg.TailnetName, "-"},
			{"R2Key", cfg.R2Key, "state.json"},
			{"Label", cfg.Label, DefaultLabel},
			{"Image", cfg.Image, DefaultImage},
			{"Region", cfg.Region, DefaultRegion},
			{"Size", cfg.Size, DefaultSize},
			{"KeyTTL", cfg.KeyTTL, DefaultKeyTTL},
			{"Interval", cfg.Interval, DefaultInterval},
			{"DrainWindow", cfg.DrainWindow, DefaultDrainWindow},
			{"MaxLifetime", cfg.MaxLifetime, DefaultMaxLifetime},
			{"BootDeadline", cfg.BootDeadline, DefaultBootDeadline},
			{"TerminateDeadline", cfg.TerminateDeadline, DefaultTerminate},
			{"RetryBase", cfg.RetryBase, DefaultRetryBase},
			{"RetryMax", cfg.RetryMax, DefaultRetryMax},
			{"SyncAttempts", cfg.SyncAttempts, DefaultSyncAttempts},
		}
		for _, c := range cases {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
			}
		}
	})

	t.Run("happy/secrets_come_from_the_environment", func(t *testing.T) {
		cfg, err := parse(t, env(), required...)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.HetznerToken.Reveal() != "hz-token" || cfg.TailnetToken.Reveal() != "ts-token" {
			t.Fatalf("tokens = %q, %q", cfg.HetznerToken.Reveal(), cfg.TailnetToken.Reveal())
		}
		if cfg.S3AccessKey.Reveal() != "access" || cfg.S3SecretKey.Reveal() != "secret" {
			t.Fatalf("storage keys = %q, %q", cfg.S3AccessKey.Reveal(), cfg.S3SecretKey.Reveal())
		}
	})
}

func TestParseFlags(t *testing.T) {
	cfg, err := parse(t, env(), append(required,
		"--tailnet", "example.com",
		"--r2-key", "prod/state.json",
		"--label", "verge=prod",
		"--image", "debian-12",
		"--region", "nbg1",
		"--size", "cx32",
		"--key-ttl", "25m",
		"--interval", "10s",
		"--drain-window", "1m",
		"--max-lifetime", "2h",
		"--boot-deadline", "3m",
		"--terminate-deadline", "2m",
		"--retry-base", "5s",
		"--retry-max", "1m",
		"--sync-attempts", "5",
	)...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.TailnetName != "example.com" {
		t.Errorf("TailnetName = %q", cfg.TailnetName)
	}
	if cfg.R2Key != "prod/state.json" {
		t.Errorf("R2Key = %q", cfg.R2Key)
	}
	if cfg.Label != "verge=prod" {
		t.Errorf("Label = %q", cfg.Label)
	}
	if cfg.Image != "debian-12" || cfg.Region != "nbg1" || cfg.Size != "cx32" {
		t.Errorf("machine = %s/%s/%s", cfg.Image, cfg.Region, cfg.Size)
	}
	if cfg.KeyTTL != 25*time.Minute || cfg.Interval != 10*time.Second {
		t.Errorf("KeyTTL = %s, Interval = %s", cfg.KeyTTL, cfg.Interval)
	}
	if cfg.MaxLifetime != 2*time.Hour || cfg.BootDeadline != 3*time.Minute {
		t.Errorf("MaxLifetime = %s, BootDeadline = %s", cfg.MaxLifetime, cfg.BootDeadline)
	}
	if cfg.SyncAttempts != 5 {
		t.Errorf("SyncAttempts = %d", cfg.SyncAttempts)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		env  func(string) string
		args []string
		want string // a substring the error must carry, so the operator knows what to fix
	}{
		{
			name: "sad/no_hetzner_token",
			env:  env(EnvHetznerToken, ""),
			args: required,
			want: EnvHetznerToken,
		},
		{
			name: "sad/no_tailnet_token",
			env:  env(EnvTailnetToken, ""),
			args: required,
			want: EnvTailnetToken,
		},
		{
			name: "sad/no_storage_credentials",
			env:  env(EnvS3AccessKey, "", EnvS3SecretKey, ""),
			args: required,
			want: EnvS3AccessKey,
		},
		{
			name: "sad/no_bucket",
			env:  env(),
			args: []string{"--r2-endpoint", "https://r2.example"},
			want: "--r2-bucket",
		},
		{
			name: "sad/no_endpoint",
			env:  env(),
			args: []string{"--r2-bucket", "state"},
			want: "--r2-endpoint",
		},
		{
			name: "bad/a_label_that_is_not_key_value",
			env:  env(),
			args: append(required, "--label", "verge"),
			want: "key=value",
		},
		{
			name: "bad/a_label_with_an_empty_value",
			env:  env(),
			args: append(required, "--label", "verge="),
			want: "key=value",
		},
		{
			name: "bad/a_zero_interval",
			env:  env(),
			args: append(required, "--interval", "0s"),
			want: "--interval",
		},
		{
			name: "bad/a_negative_drain_window",
			env:  env(),
			args: append(required, "--drain-window", "-1m"),
			want: "--drain-window",
		},
		{
			name: "bad/a_zero_max_lifetime",
			env:  env(),
			args: append(required, "--max-lifetime", "0s"),
			want: "--max-lifetime",
		},
		{
			name: "bad/a_retry_ceiling_below_its_base",
			env:  env(),
			args: append(required, "--retry-base", "1m", "--retry-max", "30s"),
			want: "--retry-max",
		},
		{
			name: "bad/no_sync_attempts",
			env:  env(),
			args: append(required, "--sync-attempts", "0"),
			want: "--sync-attempts",
		},
		{
			name: "bad/a_key_that_expires_before_the_boot_it_enrols",
			env:  env(),
			args: append(required, "--key-ttl", "1m", "--boot-deadline", "10m"),
			want: "--key-ttl",
		},
		{
			name: "bad/an_empty_machine_field",
			env:  env(),
			args: append(required, "--image", ""),
			want: "--image",
		},
		{
			name: "bad/a_stray_argument",
			env:  env(),
			args: append(required, "unexpected"),
			want: "unexpected",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(tc.args, tc.env)
			if err == nil {
				t.Fatalf("Parse accepted %v and returned %+v", tc.args, cfg)
			}
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("Parse = %v, want ErrConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not mention %q: %v", tc.want, err)
			}
			if cfg != nil {
				t.Fatal("Parse returned a config with an error")
			}
		})
	}
}

// TestMissingEverything names all of it at once: an operator with no environment should
// not have to run the binary four times to find out what it needs.
func TestMissingEverything(t *testing.T) {
	_, err := Parse(nil, func(string) string { return "" })
	if err == nil {
		t.Fatal("Parse accepted an empty environment and no arguments")
	}
	for _, want := range []string{EnvHetznerToken, EnvTailnetToken, EnvS3AccessKey, EnvS3SecretKey, "--r2-bucket", "--r2-endpoint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

// TestFlagsAreNotSecrets: the same credentials passed as flags must not be accepted,
// because that is the one place they would be visible to every user on the box.
func TestFlagsAreNotSecrets(t *testing.T) {
	for _, args := range [][]string{
		append(required, "--hetzner-token", "leaked"),
		append(required, "--tailnet-token", "leaked"),
		append(required, "--s3-secret-key", "leaked"),
	} {
		t.Run(args[len(args)-2], func(t *testing.T) {
			_, err := Parse(args, env())
			if err == nil {
				t.Fatalf("Parse accepted %s as a flag", args[len(args)-2])
			}
			// flag's own error is the right one: the flag does not exist.
			if !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("Parse = %v, want an undefined-flag error", err)
			}
		})
	}
}

func TestSecretsDoNotPrint(t *testing.T) {
	cfg, err := parse(t, env(), required...)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// render goes through a builder rather than Sprintf so that each case is a verb
	// being tried on a value, not a String() call that happens to look like one.
	render := func(format string, args ...any) string {
		var b strings.Builder
		fmt.Fprintf(&b, format, args...)
		return b.String()
	}

	secrets := []string{"hz-token", "ts-token", "access", "secret"}
	renderings := map[string]string{
		"%v":  render("%v", cfg),
		"%+v": render("%+v", *cfg),
		"%s":  render("%s", *cfg),
		"%#v": render("%#v", *cfg),
		"pointers": render("%+v %v %v %v %v", &cfg.HetznerToken, cfg.HetznerToken,
			cfg.TailnetToken, cfg.S3AccessKey, cfg.S3SecretKey),
		"secret alone": render("%v %+v %s %d", cfg.HetznerToken, cfg.HetznerToken,
			cfg.HetznerToken, len(cfg.HetznerToken)),
	}

	for name, out := range renderings {
		for _, secret := range secrets {
			if strings.Contains(out, secret) {
				t.Errorf("%s printed the secret %q: %s", name, secret, out)
			}
		}
	}
	// And the redaction itself is stable, or a log line would be a coincidence.
	if got := fmt.Sprintf("%v", cfg.HetznerToken); got != Redacted {
		t.Fatalf("a Secret prints as %q, want %q", got, Redacted)
	}
}

func TestUsage(t *testing.T) {
	t.Run("happy/a_usage_writer_receives_the_text_and_the_flag_list", func(t *testing.T) {
		var out strings.Builder
		_, err := Parse([]string{"-h"}, env(), WithUsage(&out))
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("Parse(-h) = %v, want flag.ErrHelp", err)
		}
		text := out.String()
		if !strings.Contains(text, EnvHetznerToken) {
			t.Errorf("the usage text does not name the token variables: %s", text)
		}
		if !strings.Contains(text, "-r2-bucket") {
			t.Errorf("the usage text does not list the flags: %s", text)
		}
		// The usage text is the first thing an operator reads; it must not show a value.
		if strings.Contains(text, "[redacted]") {
			t.Error("the usage text contains a secret")
		}
	})

	t.Run("sad/an_unknown_flag_is_an_error_not_a_default", func(t *testing.T) {
		var out strings.Builder
		_, err := Parse([]string{"--nonsense"}, env(), WithUsage(&out))
		if err == nil {
			t.Fatal("Parse accepted an unknown flag")
		}
		if !errors.Is(err, flag.ErrHelp) && !strings.Contains(err.Error(), "nonsense") {
			t.Fatalf("Parse = %v", err)
		}
	})
}
