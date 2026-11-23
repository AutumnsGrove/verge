package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

const (
	testBucket = "verge-state"
	testKey    = "prod/state.json"
)

var base = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// newStore points a real S3 client at an httptest server.
//
// The client is the SDK's own, signing real SigV4 requests against a fake endpoint:
// the seam under test is HTTP, so there is no mocked API object in sight. Path style
// is what R2 expects when the bucket is in the host position it does not control, and
// it also makes the request path readable in an assertion.
func newStore(t *testing.T, h http.HandlerFunc) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("access", "secret", ""),
		HTTPClient:  srv.Client(),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})

	s, err := New(Config{Bucket: testBucket, Key: testKey, API: client})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, srv
}

// document is a document with something in every field this package has to preserve.
//
// It carries the two fields the rekey added — Machine.Name and NextMachineNumber —
// because a store that round-tripped everything except those would leave a restart
// unable to name the machines it owns, and that failure would appear as a tick that
// cannot ask the network anything rather than as a storage bug.
func document() firefly.Document {
	return firefly.Document{
		Version:           firefly.DocumentVersion,
		NextLeaseNumber:   7,
		NextMachineNumber: 3,
		Machines: []firefly.Machine{{
			ID:        firefly.MachineID("hetzner:42"),
			Name:      "verge-machine-2",
			State:     firefly.Ready,
			CreatedAt: base,
			Deadline:  base.Add(4 * time.Hour),
			Since:     base.Add(time.Minute),
		}},
		Leases: []firefly.Lease{{
			ID:      firefly.LeaseID("lease-7"),
			Machine: firefly.MachineID("hetzner:42"),
			Holder:  "session:01J8",
			Kind:    firefly.Session,
			Start:   base,
			TTL:     time.Hour,
		}},
	}
}

func TestRoundTrip(t *testing.T) {
	t.Run("happy/what_is_saved_is_what_is_loaded", func(t *testing.T) {
		var body []byte
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPut:
				body, _ = io.ReadAll(r.Body)
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				if got := r.URL.Path; got != "/"+testBucket+"/"+testKey {
					t.Errorf("PUT path = %q, want /%s/%s", got, testBucket, testKey)
				}
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				if got := r.URL.Path; got != "/"+testBucket+"/"+testKey {
					t.Errorf("GET path = %q, want /%s/%s", got, testBucket, testKey)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
			default:
				t.Errorf("%s %s, want GET or PUT", r.Method, r.URL.Path)
			}
		})

		ctx := context.Background()
		want := document()
		if err := s.Save(ctx, want); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		// DeepEqual rather than a field-by-field comparison: the point of a store is
		// fidelity, and a field nobody thought to assert is exactly the field that
		// comes back empty.
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("round trip changed the document:\n got %+v\nwant %+v", got, want)
		}
		if got.NextMachineNumber != want.NextMachineNumber {
			t.Fatalf("NextMachineNumber = %d, want %d", got.NextMachineNumber, want.NextMachineNumber)
		}
		if len(got.Machines) != 1 || got.Machines[0].Name != "verge-machine-2" {
			t.Fatalf("the machine name did not survive: %+v", got.Machines)
		}
		// A stored document that does not validate would be refused on load, so the
		// fixture is only useful if it is legal.
		if err := got.Validate(); err != nil {
			t.Fatalf("the round-tripped document does not validate: %v", err)
		}
	})

	t.Run("happy/the_body_is_the_document", func(t *testing.T) {
		// The object is inspectable on purpose: it is the thing a human looks at when
		// a machine will not fade.
		var raw map[string]any
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("the saved body is not JSON: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		})
		if err := s.Save(context.Background(), document()); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if raw["Version"] != float64(firefly.DocumentVersion) {
			t.Fatalf("Version = %v, want %d", raw["Version"], firefly.DocumentVersion)
		}
	})
}

func TestLoadAbsent(t *testing.T) {
	t.Run("happy/a_missing_object_is_a_fresh_deployment", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>
<Key>prod/state.json</Key></Error>`)
		})

		if _, err := s.Load(context.Background()); !errors.Is(err, firefly.ErrNoDocument) {
			t.Fatalf("Load of a missing object = %v, want ErrNoDocument", err)
		}
	})

	// A missing bucket is a deployment that was torn down or never created. Reading it
	// as "no document yet" would hide that behind the ordinary first-run path.
	t.Run("bad/a_missing_bucket_is_not_an_absent_document", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message></Error>`)
		})

		_, err := s.Load(context.Background())
		if err == nil {
			t.Fatal("Load of a missing bucket reported an absent document")
		}
		if errors.Is(err, firefly.ErrNoDocument) {
			t.Fatalf("a missing bucket was read as no document yet: %v", err)
		}
	})
}

func TestErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("sad/a_forbidden_read", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		})
		_, err := s.Load(ctx)
		if err == nil || errors.Is(err, firefly.ErrNoDocument) {
			t.Fatalf("Load of a forbidden object = %v, want an error that is not ErrNoDocument", err)
		}
	})

	t.Run("sad/a_forbidden_write", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		})
		if err := s.Save(ctx, document()); err == nil {
			t.Fatal("Save reported success for a refused write")
		}
	})

	t.Run("bad/a_body_that_is_not_json", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"Version":2,`)
		})
		if _, err := s.Load(ctx); err == nil {
			t.Fatal("Load accepted a truncated document")
		}
	})

	// An empty object is a document that was written and did not arrive. Reporting it
	// as "no document yet" is how a restart adopts nothing and then deletes everything.
	t.Run("bad/an_empty_body", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		_, err := s.Load(ctx)
		if err == nil || errors.Is(err, firefly.ErrNoDocument) {
			t.Fatalf("Load of an empty body = %v, want an error that is not ErrNoDocument", err)
		}
	})

	t.Run("bad/a_cancelled_context", func(t *testing.T) {
		s, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a cancelled context reached the store")
		})
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		if _, err := s.Load(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load = %v, want context.Canceled", err)
		}
		if err := s.Save(cancelled, document()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Save = %v, want context.Canceled", err)
		}
	})
}

func TestNew(t *testing.T) {
	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("access", "secret", ""),
	})

	cases := []struct {
		name  string
		cfg   Config
		isErr bool
	}{
		{"happy/a_complete_configuration", Config{Bucket: testBucket, Key: testKey, API: client}, false},
		{"sad/no_api", Config{Bucket: testBucket, Key: testKey}, true},
		{"sad/no_bucket", Config{Key: testKey, API: client}, true},
		{"sad/no_key", Config{Bucket: testBucket, API: client}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.cfg)
			if tc.isErr {
				if !errors.Is(err, ErrConfig) {
					t.Fatalf("New = %v, want ErrConfig", err)
				}
				if s != nil {
					t.Fatal("New returned a store with an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if s == nil {
				t.Fatal("New returned no store")
			}
		})
	}
}

// TestStoreIsUsableByFirefly drives the store through the interface firefly holds,
// which is the only reason the package exists.
func TestStoreIsUsableByFirefly(t *testing.T) {
	var stored []byte
	inner, _ := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			stored, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Write(stored)
		}
	})

	cfg := firefly.Config{
		Store: inner, Cloud: nopCloud{}, Network: nopNetwork{}, Syncer: nopSyncer{},
		Label: "verge=machine", MaxLifetime: 4 * time.Hour, DrainWindow: 15 * time.Minute,
		BootDeadline: 10 * time.Minute, SyncAttempts: 3,
		TerminateDeadline: 5 * time.Minute, RetryBase: 30 * time.Second, RetryMax: 5 * time.Minute,
	}
	first, err := firefly.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// A fresh store holds no document, and firefly must treat that as empty rather
	// than as a reason to stop.
	if err := first.Load(ctx); err != nil {
		t.Fatalf("Load on an empty store: %v", err)
	}

	// A restart reads back what the previous process wrote. This is the whole point of
	// the package: the machines survive the process.
	id, err := first.Provision(ctx, firefly.Spec{}, base)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	second, err := firefly.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap, err := second.Snapshot(ctx, base)
	if err != nil {
		t.Fatalf("Snapshot after a restart: %v", err)
	}
	if len(snap.Machines) != 1 || snap.Machines[0].ID != id {
		t.Fatalf("the restarted firefly sees %+v, want the machine it created", snap.Machines)
	}
	// And the name came back with it, or the restarted process could not ask the
	// network about the machine it just recovered.
	if snap.Machines[0].Name == "" {
		t.Fatal("the machine came back without its name")
	}
}

// The stubs exist so this test talks to a real store without a real cloud: the
// provider calls are never made, because the test only provisions and snapshots.
type nopCloud struct{}

func (nopCloud) Create(context.Context, firefly.Spec) (firefly.MachineID, error) {
	return firefly.MachineID("hetzner:1"), nil
}
func (nopCloud) Describe(context.Context, firefly.MachineID) (firefly.CloudMachine, error) {
	return firefly.CloudMachine{}, nil
}
func (nopCloud) List(context.Context, string) ([]firefly.CloudMachine, error) { return nil, nil }
func (nopCloud) Delete(context.Context, firefly.MachineID) error              { return nil }

type nopNetwork struct{}

func (nopNetwork) Joined(context.Context, string) (bool, error)     { return false, nil }
func (nopNetwork) NodeExists(context.Context, string) (bool, error) { return false, nil }
func (nopNetwork) Forget(context.Context, string) error             { return nil }

type nopSyncer struct{}

func (nopSyncer) Sync(context.Context, firefly.MachineID) error { return nil }
