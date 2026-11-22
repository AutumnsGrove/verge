package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

const token = "test-token"

// serverJSON is a recorded shape, so the tests fail when the decode changes rather
// than when someone remembers.
const serverJSON = `{
  "id": 42,
  "name": "verge",
  "status": %q,
  "created": "2026-11-22T12:00:00+00:00",
  "labels": {"verge": "machine"},
  "public_net": {"ipv4": {"ip": "10.0.0.42"}}
}`

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(token, WithBaseURL(srv.URL)), srv
}

func TestCreate(t *testing.T) {
	t.Run("happy/returns_a_machine_id", func(t *testing.T) {
		var got map[string]any
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/servers" {
				t.Errorf("%s %s, want POST /servers", r.Method, r.URL.Path)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer "+token {
				t.Errorf("Authorization = %q", auth)
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("the request body is not JSON: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"server":%s}`, fmt.Sprintf(serverJSON, "starting"))
		})

		id, err := c.Create(context.Background(), firefly.Spec{
			Image:     "ubuntu-24.04",
			Size:      "cx22",
			Region:    "fsn1",
			CloudInit: []byte("#cloud-config\n"),
			Labels:    map[string]string{"verge": "machine"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id != "hetzner:42" {
			t.Fatalf("id = %q, want hetzner:42", id)
		}

		if got["image"] != "ubuntu-24.04" || got["server_type"] != "cx22" || got["location"] != "fsn1" {
			t.Fatalf("the request did not carry the spec: %v", got)
		}
		if got["user_data"] != "#cloud-config\n" {
			t.Fatalf("user_data = %q, want the rendered cloud-init", got["user_data"])
		}
	})

	t.Run("sad/api_error_is_returned_with_its_code", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Invalid token"}}`)
		})

		_, err := c.Create(context.Background(), firefly.Spec{Image: "ubuntu-24.04"})
		if err == nil {
			t.Fatal("Create succeeded against a 401")
		}
		var e errAPI
		if !errors.As(err, &e) || e.Status != http.StatusUnauthorized || e.Code != "unauthorized" {
			t.Fatalf("Create error = %v, want the provider's 401", err)
		}
	})
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		wantStatus firefly.ProviderStatus
		isErr      bool
	}{
		{"happy/initializing", "initializing", firefly.ProviderCreated, false},
		{"happy/starting", "starting", firefly.ProviderStarting, false},
		{"happy/running", "running", firefly.ProviderRunning, false},
		{"happy/stopping", "stopping", firefly.ProviderStopping, false},
		{"happy/off", "off", firefly.ProviderStopping, false},
		{"happy/deleting", "deleting", firefly.ProviderStopping, false},
		{"bad/unknown_status_is_an_error", "ascending", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/servers/42" {
					t.Errorf("path = %q, want /servers/42", r.URL.Path)
				}
				fmt.Fprintf(w, `{"server":%s}`, fmt.Sprintf(serverJSON, tc.status))
			})

			m, err := c.Describe(context.Background(), "hetzner:42")
			if tc.isErr {
				if err == nil {
					t.Fatal("Describe accepted an unknown status")
				}
				return
			}
			if err != nil {
				t.Fatalf("Describe: %v", err)
			}
			if m.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", m.Status, tc.wantStatus)
			}
			if m.PublicIP != "10.0.0.42" {
				t.Fatalf("PublicIP = %q", m.PublicIP)
			}
			if m.Labels["verge"] != "machine" {
				t.Fatalf("Labels = %v", m.Labels)
			}
			if m.CreatedAt.IsZero() {
				t.Fatal("CreatedAt was not parsed")
			}
		})
	}

	t.Run("sad/not_found_is_the_one_way_to_report_absence", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"server not found"}}`)
		})
		_, err := c.Describe(context.Background(), "hetzner:42")
		if !errors.Is(err, firefly.ErrNoSuchMachine) {
			t.Fatalf("Describe of a missing machine = %v, want ErrNoSuchMachine", err)
		}
	})

	t.Run("bad/another_providers_id", func(t *testing.T) {
		c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a foreign id reached the provider")
		})
		_ = srv
		if _, err := c.Describe(context.Background(), "fake-001"); err == nil {
			t.Fatal("Describe accepted a machine id from another provider")
		}
	})
}

func TestList(t *testing.T) {
	t.Run("happy/uses_the_label_selector", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("label_selector"); got != "verge=machine" {
				t.Errorf("label_selector = %q", got)
			}
			fmt.Fprintf(w, `{"servers":[%s,%s]}`,
				fmt.Sprintf(serverJSON, "running"),
				`{"id":43,"status":"off","created":"2026-11-22T11:00:00+00:00","labels":{}}`)
		})

		got, err := c.List(context.Background(), "verge=machine")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List returned %d machines, want 2", len(got))
		}
		if got[0].Status != firefly.ProviderRunning || got[1].Status != firefly.ProviderStopping {
			t.Fatalf("statuses = %s, %s", got[0].Status, got[1].Status)
		}
	})

	t.Run("happy/empty_list_is_not_an_error", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Errorf("an empty label produced a query: %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"servers":[]}`)
		})
		got, err := c.List(context.Background(), "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("List returned %d machines", len(got))
		}
	})

	t.Run("bad/malformed_json", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"servers":[{"id":`)
		})
		if _, err := c.List(context.Background(), ""); err == nil {
			t.Fatal("List accepted malformed JSON")
		}
	})
}

func TestDelete(t *testing.T) {
	t.Run("happy/deletes_the_server", func(t *testing.T) {
		var called bool
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			if r.Method != http.MethodDelete || r.URL.Path != "/servers/42" {
				t.Errorf("%s %s, want DELETE /servers/42", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		})

		if err := c.Delete(context.Background(), "hetzner:42"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if !called {
			t.Fatal("Delete did not reach the provider")
		}
	})

	t.Run("happy/a_404_is_success", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"server not found"}}`)
		})
		if err := c.Delete(context.Background(), "hetzner:42"); err != nil {
			t.Fatalf("Delete of a missing machine = %v, want nil", err)
		}
	})

	t.Run("sad/other_errors_are_not_swallowed", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
		})
		err := c.Delete(context.Background(), "hetzner:42")
		if err == nil {
			t.Fatal("Delete swallowed a 429")
		}
		var e errAPI
		if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
			t.Fatalf("Delete error = %v, want the provider's 429", err)
		}
	})
}

// TestCancelledContext covers the path a shutdown takes: the request must not be
// issued, and the error must be the context's.
func TestCancelledContext(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a cancelled context still reached the provider")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("List with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestServerIDRoundTrip pins the id format: it is the only place a provider id is
// encoded, and a change here would silently reinterpret every stored machine.
func TestServerIDRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		in    firefly.MachineID
		want  int64
		isErr bool
	}{
		{"happy/round_trip", "hetzner:42", 42, false},
		{"happy/large", "hetzner:9223372036854775807", 9223372036854775807, false},
		{"bad/prefix_only", "hetzner:", 0, true},
		{"bad/other_provider", "fake-001", 0, true},
		{"bad/not_a_number", "hetzner:abc", 0, true},
		{"bad/empty", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serverID(tc.in)
			if tc.isErr {
				if err == nil {
					t.Fatalf("serverID(%q) accepted a bad id", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("serverID(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("serverID(%q) = %d, want %d", tc.in, got, tc.want)
			}
			if back := machineID(got); back != tc.in {
				t.Fatalf("round trip of %q gave %q", tc.in, back)
			}
		})
	}
}
