// The syncer's tests live in the external package, because the only thing worth
// testing here is the boundary: a fetcher that returns what a machine sent, and a
// sink that stores what it was handed. Both are test doubles at a seam with a
// network on the far side, which is the one place this repository allows them --
// see docs/testing.md, "What this repository deliberately does not do".
package syncer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/syncer"
)

// fetcher is the transport boundary: it hands back whatever the machine sent.
type fetcher struct {
	body  []byte
	err   error
	calls []firefly.MachineID
}

func (f *fetcher) Fetch(ctx context.Context, id firefly.MachineID) (io.ReadCloser, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

// put is one object the sink was asked to store.
type put struct {
	key  string
	body []byte
	size int64
}

type sink struct {
	puts []put
	err  error
}

func (s *sink) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	if s.err != nil {
		return s.err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.puts = append(s.puts, put{key: key, body: b, size: size})
	return nil
}

func newSyncer(t *testing.T, f *fetcher, s *sink) *syncer.Syncer {
	t.Helper()
	sy, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: "state"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sy
}

// TestSync saves the workspace, and is the reason the type exists.
func TestSync(t *testing.T) {
	t.Run("happy/workspace_is_stored_under_the_machine", func(t *testing.T) {
		f := &fetcher{body: []byte("a tarball, honestly")}
		s := &sink{}
		sy := newSyncer(t, f, s)

		if err := sy.Sync(context.Background(), "fake-001"); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		if len(s.puts) != 1 {
			t.Fatalf("stored %d objects, want 1", len(s.puts))
		}
		if got, want := s.puts[0].key, "state/fake-001.tar"; got != want {
			t.Fatalf("key = %q, want %q", got, want)
		}
		if !bytes.Equal(s.puts[0].body, f.body) {
			t.Fatalf("stored %q, want the fetched bytes", s.puts[0].body)
		}
		if got := s.puts[0].size; got != int64(len(f.body)) {
			t.Fatalf("size = %d, want %d", got, len(f.body))
		}
	})

	t.Run("happy/nothing_to_save_is_not_a_failure", func(t *testing.T) {
		// The contract says nil when there was nothing to save, which is the
		// ordinary case for a machine that never became ready.
		f := &fetcher{}
		s := &sink{}
		sy := newSyncer(t, f, s)

		if err := sy.Sync(context.Background(), "fake-001"); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if len(s.puts) != 0 {
			t.Fatalf("stored %d objects for an empty workspace, want 0", len(s.puts))
		}
	})

	t.Run("happy/spool_file_is_removed", func(t *testing.T) {
		// A workspace is spooled to a real file rather than buffered, so the file
		// it leaves behind is a file per sync, forever. Point TMPDIR at somewhere
		// this test owns and require it empty afterwards.
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)

		f := &fetcher{body: []byte("a tarball, honestly")}
		s := &sink{}
		sy := newSyncer(t, f, s)

		if err := sy.Sync(context.Background(), "fake-001"); err != nil {
			t.Fatalf("Sync: %v", err)
		}

		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(ents) != 0 {
			t.Fatalf("left %d files behind in %s: %v", len(ents), dir, ents)
		}
	})

	t.Run("sad/machine_is_gone", func(t *testing.T) {
		// A fetch that fails is not "nothing to save": firefly keeps the machine
		// and retries, and only the cap may abandon it.
		gone := errors.New("no route to the machine")
		f := &fetcher{err: gone}
		s := &sink{}
		sy := newSyncer(t, f, s)

		err := sy.Sync(context.Background(), "fake-001")
		if !errors.Is(err, gone) {
			t.Fatalf("Sync = %v, want it to wrap %v", err, gone)
		}
		if len(s.puts) != 0 {
			t.Fatalf("stored %d objects for a failed fetch, want 0", len(s.puts))
		}
	})

	t.Run("sad/sink_refuses", func(t *testing.T) {
		refused := errors.New("the bucket said no")
		f := &fetcher{body: []byte("a tarball, honestly")}
		s := &sink{err: refused}
		sy := newSyncer(t, f, s)

		err := sy.Sync(context.Background(), "fake-001")
		if !errors.Is(err, refused) {
			t.Fatalf("Sync = %v, want it to wrap %v", err, refused)
		}
	})

	t.Run("sad/workspace_is_over_the_cap", func(t *testing.T) {
		// An unbounded copy from a source this process does not control is how a
		// machine fills the control plane's disk. The body is refused, not stored.
		// The cap is a Config field, so this one byte over is one byte over.
		f := &fetcher{body: bytes.Repeat([]byte("x"), 1025)}
		s := &sink{}
		sy, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: "state", MaxBytes: 1024})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		err = sy.Sync(context.Background(), "fake-001")
		if !errors.Is(err, syncer.ErrWorkspaceTooLarge) {
			t.Fatalf("Sync = %v, want %v", err, syncer.ErrWorkspaceTooLarge)
		}
		if len(s.puts) != 0 {
			t.Fatalf("stored %d objects for an oversized workspace, want 0", len(s.puts))
		}
	})

	t.Run("bad/machine_id_escapes_the_prefix", func(t *testing.T) {
		// The id comes from the provider, and it lands in an object key. A key is
		// not a path, but a store that treats it as one would put one machine's
		// workspace where another's belongs.
		for _, id := range []firefly.MachineID{"", "../elsewhere", "a/b", "..", "fake 001"} {
			f := &fetcher{body: []byte("a tarball, honestly")}
			s := &sink{}
			sy := newSyncer(t, f, s)

			if err := sy.Sync(context.Background(), id); err == nil {
				t.Fatalf("Sync(%q) = nil, want a refusal", id)
			}
			if len(s.puts) != 0 {
				t.Fatalf("Sync(%q) stored an object, want a refusal", id)
			}
		}
	})
}

// TestNew refuses a syncer that cannot work, rather than failing on the first
// machine that needed saving.
func TestNew(t *testing.T) {
	f := &fetcher{}
	s := &sink{}

	t.Run("bad/no_fetcher", func(t *testing.T) {
		if _, err := syncer.New(syncer.Config{Sink: s, Prefix: "state"}); !errors.Is(err, syncer.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, syncer.ErrConfig)
		}
	})

	t.Run("bad/no_sink", func(t *testing.T) {
		if _, err := syncer.New(syncer.Config{Fetcher: f, Prefix: "state"}); !errors.Is(err, syncer.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, syncer.ErrConfig)
		}
	})

	t.Run("bad/no_prefix", func(t *testing.T) {
		// Without a prefix, every deployment's workspaces land in one flat space,
		// and two environments share an object the moment an id collides.
		for _, p := range []string{"", "/"} {
			if _, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: p}); !errors.Is(err, syncer.ErrConfig) {
				t.Fatalf("New(Prefix=%q) = %v, want %v", p, err, syncer.ErrConfig)
			}
		}
	})

	t.Run("happy/trailing_slash_is_the_same_prefix", func(t *testing.T) {
		// A prefix written "state/" must not produce "state//fake-001.tar".
		f := &fetcher{body: []byte("a tarball, honestly")}
		s := &sink{}
		sy, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: "state/"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := sy.Sync(context.Background(), "fake-001"); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if got, want := s.puts[0].key, "state/fake-001.tar"; got != want {
			t.Fatalf("key = %q, want %q", got, want)
		}
	})

	t.Run("bad/prefix_looking_like_a_path_escape", func(t *testing.T) {
		if _, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: "../state"}); !errors.Is(err, syncer.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, syncer.ErrConfig)
		}
	})

	t.Run("bad/cap_below_a_tarball_header", func(t *testing.T) {
		if _, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: "state", MaxBytes: 512}); !errors.Is(err, syncer.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, syncer.ErrConfig)
		}
	})

	t.Run("bad/negative_cap", func(t *testing.T) {
		if _, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: "state", MaxBytes: -1}); !errors.Is(err, syncer.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, syncer.ErrConfig)
		}
	})
}

// TestKeyIsStable is the property the object's name has to have: the same machine
// always lands on the same object, because restore reads by name and a key that
// moved would lose the last good copy.
func TestKeyIsStable(t *testing.T) {
	t.Run("happy/two_syncs_of_one_machine_share_a_key", func(t *testing.T) {
		for _, prefix := range []string{"state", "state/", "verge/prod"} {
			f := &fetcher{body: []byte("one")}
			s := &sink{}
			sy, err := syncer.New(syncer.Config{Fetcher: f, Sink: s, Prefix: prefix})
			if err != nil {
				t.Fatalf("New(%q): %v", prefix, err)
			}
			for i := 0; i < 2; i++ {
				if err := sy.Sync(context.Background(), "fake-001"); err != nil {
					t.Fatalf("Sync: %v", err)
				}
			}
			if len(s.puts) != 2 || s.puts[0].key != s.puts[1].key {
				t.Fatalf("prefix %q: keys = %q and %q, want the same key", prefix, s.puts[0].key, s.puts[1].key)
			}
			if strings.HasPrefix(s.puts[0].key, "/") || strings.Contains(s.puts[0].key, "//") {
				t.Fatalf("prefix %q: key %q is not a clean object key", prefix, s.puts[0].key)
			}
		}
	})
}
