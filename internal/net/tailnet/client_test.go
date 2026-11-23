package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

const token = "test-token"

// devicesJSON is a recorded response shape, so the tests fail when the decode
// changes rather than when someone remembers to look.
func devicesJSON(devices ...string) string {
	return `{"devices":[` + strings.Join(devices, ",") + `]}`
}

// device is one entry in a listing. The defaults describe a machine that joined
// normally: registered, approved, and talking to the control plane.
func device(nodeID, hostname string, opts ...func(map[string]any)) string {
	d := map[string]any{
		"id":                 "1",
		"nodeId":             nodeID,
		"name":               hostname + ".tailnet.ts.net.",
		"hostname":           hostname,
		"authorized":         true,
		"connectedToControl": true,
	}
	for _, o := range opts {
		o(d)
	}
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func offline(d map[string]any)    { d["connectedToControl"] = false }
func unapproved(d map[string]any) { d["authorized"] = false }
func noHostname(d map[string]any) { delete(d, "hostname") }

// newTestClient points a client at a handler and reports the client, so a test can
// say what it wants answered and what it wants requested in the same place.
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(token, WithBaseURL(srv.URL)), srv
}

// listOnly answers every request with the given device entries, and fails the test
// on anything that is not a listing.
func listOnly(t *testing.T, devices string) (*Client, *httptest.Server) {
	t.Helper()
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s, want GET", r.Method, r.URL.Path)
		}
		if r.URL.Path != "/tailnet/-/devices" {
			t.Errorf("path = %q, want /tailnet/-/devices", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer "+token {
			t.Errorf("Authorization = %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, devices)
	})
}

func TestJoined(t *testing.T) {
	cases := []struct {
		name       string
		devices    string
		wantJoined bool
		wantExists bool
	}{
		{
			name:       "happy/registered_approved_and_connected",
			devices:    devicesJSON(device("n1", "verge-machine-1")),
			wantJoined: true, wantExists: true,
		},
		{
			// A device whose agent is not talking to the control plane exists as a
			// record and is of no use to anyone. This is the pair the two methods
			// exist to separate.
			name:       "sad/registered_but_not_connected",
			devices:    devicesJSON(device("n1", "verge-machine-1", offline)),
			wantJoined: false, wantExists: true,
		},
		{
			// Awaiting approval: registered, present, not a member. Firefly cannot
			// tell this apart from a machine that never joined, which is why the boot
			// deadline is what bounds the cost of the misconfiguration.
			name:       "sad/registered_but_not_authorized",
			devices:    devicesJSON(device("n1", "verge-machine-1", unapproved)),
			wantJoined: false, wantExists: true,
		},
		{
			name:       "sad/no_device_has_that_name",
			devices:    devicesJSON(device("n1", "verge-machine-2")),
			wantJoined: false, wantExists: false,
		},
		{
			name:       "happy/an_empty_tailnet_is_not_an_error",
			devices:    devicesJSON(),
			wantJoined: false, wantExists: false,
		},
		{
			// Hetzner accepts mixed case in a server name and a tailnet reports the
			// hostname it was given; a case-sensitive match would leave a machine with
			// a caller-chosen name permanently unjoined.
			name:       "happy/the_match_ignores_case",
			devices:    devicesJSON(device("n1", "VERGE-MACHINE-1")),
			wantJoined: true, wantExists: true,
		},
		{
			// The MagicDNS name is the fallback for a device that reported no
			// hostname: it is the hostname plus the tailnet's domain, so its first
			// label is still the name worth matching.
			name:       "happy/falls_back_to_the_magicdns_label",
			devices:    devicesJSON(device("n1", "verge-machine-1", noHostname)),
			wantJoined: true, wantExists: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := listOnly(t, tc.devices)
			ctx := context.Background()

			joined, err := c.Joined(ctx, "verge-machine-1")
			if err != nil {
				t.Fatalf("Joined: %v", err)
			}
			if joined != tc.wantJoined {
				t.Errorf("Joined = %v, want %v", joined, tc.wantJoined)
			}

			exists, err := c.NodeExists(ctx, "verge-machine-1")
			if err != nil {
				t.Fatalf("NodeExists: %v", err)
			}
			if exists != tc.wantExists {
				t.Errorf("NodeExists = %v, want %v", exists, tc.wantExists)
			}

			// The contradiction the split exists to prevent: a usable node that is
			// not there, or a node that is usable while its record is missing.
			if joined && !exists {
				t.Error("Joined = true while NodeExists = false")
			}
		})
	}
}

// TestAmbiguousName: two devices with one hostname is a configuration problem, not
// something to resolve by picking the first. Picking for Joined marks a machine ready
// that is not; picking for Forget deletes a node firefly did not create.
func TestAmbiguousName(t *testing.T) {
	ctx := context.Background()
	devices := devicesJSON(
		device("n1", "verge-machine-1"),
		device("n2", "verge-machine-1"),
	)

	calls := []struct {
		name string
		call func(*Client) error
	}{
		{"bad/joined", func(c *Client) error { _, err := c.Joined(ctx, "verge-machine-1"); return err }},
		{"bad/node_exists", func(c *Client) error { _, err := c.NodeExists(ctx, "verge-machine-1"); return err }},
		{"bad/forget", func(c *Client) error { return c.Forget(ctx, "verge-machine-1") }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := listOnly(t, devices)
			err := tc.call(c)
			if !errors.Is(err, ErrAmbiguousNode) {
				t.Fatalf("%s = %v, want ErrAmbiguousNode", tc.name, err)
			}
			// The message has to name both devices, because the fix is a human
			// deciding which one is real.
			if msg := err.Error(); !strings.Contains(msg, "n1") || !strings.Contains(msg, "n2") {
				t.Fatalf("the error does not name both devices: %v", err)
			}
		})
	}
}

func TestForget(t *testing.T) {
	t.Run("happy/deletes_the_device_by_node_id", func(t *testing.T) {
		var deleted string
		var deletes int
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				fmt.Fprint(w, devicesJSON(device("n1", "verge-machine-1")))
			case http.MethodDelete:
				deletes++
				deleted = r.URL.Path
				w.WriteHeader(http.StatusOK)
			default:
				t.Errorf("%s %s, want GET or DELETE", r.Method, r.URL.Path)
			}
		})

		if err := c.Forget(context.Background(), "verge-machine-1"); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if deletes != 1 {
			t.Fatalf("%d deletes, want 1", deletes)
		}
		if want := "/device/n1"; deleted != want {
			t.Fatalf("deleted %q, want %q", deleted, want)
		}
	})

	t.Run("happy/falls_back_to_the_legacy_id", func(t *testing.T) {
		var deleted string
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fmt.Fprint(w, devicesJSON(device("", "verge-machine-1")))
				return
			}
			deleted = r.URL.Path
		})

		if err := c.Forget(context.Background(), "verge-machine-1"); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if want := "/device/1"; deleted != want {
			t.Fatalf("deleted %q, want %q", deleted, want)
		}
	})

	// The ordinary case for an ephemeral key: the record went when the machine
	// disconnected, so the repair has nothing to do. A second Forget is not an error.
	t.Run("happy/a_node_that_is_already_gone_is_success", func(t *testing.T) {
		var deletes int
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				deletes++
			}
			fmt.Fprint(w, devicesJSON())
		})

		for i := 0; i < 2; i++ {
			if err := c.Forget(context.Background(), "verge-machine-1"); err != nil {
				t.Fatalf("Forget #%d = %v, want nil", i+1, err)
			}
		}
		if deletes != 0 {
			t.Fatalf("%d deletes were issued for a node that was not there", deletes)
		}
	})

	t.Run("happy/a_delete_that_raced_the_tailnet_is_success", func(t *testing.T) {
		// The listing said the device was there; the delete found it already gone,
		// which is the same outcome and not something the caller can distinguish.
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fmt.Fprint(w, devicesJSON(device("n1", "verge-machine-1")))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})

		if err := c.Forget(context.Background(), "verge-machine-1"); err != nil {
			t.Fatalf("Forget = %v, want nil for a device that had already gone", err)
		}
	})

	t.Run("sad/a_refused_delete_is_an_error", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fmt.Fprint(w, devicesJSON(device("n1", "verge-machine-1")))
				return
			}
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"the tailnet does not allow that"}`)
		})

		err := c.Forget(context.Background(), "verge-machine-1")
		if err == nil {
			t.Fatal("Forget reported success for a refused delete")
		}
		// The provider's own words reach the operator: a 403 here is a token scope
		// problem, and "forbidden" alone would not say so.
		if !strings.Contains(err.Error(), "does not allow that") {
			t.Fatalf("the refusal lost its message: %v", err)
		}
	})
}

// TestErrorsAreNotAbsence: a tailnet that cannot be read is not an empty tailnet.
// Reporting false here would let a machine that never joined be confirmed gone, and
// would let a TERMINATING machine reach ABSENT with its node still in place.
func TestErrorsAreNotAbsence(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"sad/expired_token", http.StatusUnauthorized, `{"message":"API key expired"}`},
		{"sad/the_tailnet_does_not_exist", http.StatusNotFound, `{"message":"tailnet not found"}`},
		{"sad/the_control_plane_is_unwell", http.StatusInternalServerError, ``},
		{"bad/malformed_json", http.StatusOK, `{"devices":[{"nodeId":`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})

			if joined, err := c.Joined(ctx, "verge-machine-1"); err == nil {
				t.Fatalf("Joined = %v with no error; a failing tailnet is not an unjoined machine", joined)
			}
			if exists, err := c.NodeExists(ctx, "verge-machine-1"); err == nil {
				t.Fatalf("NodeExists = %v with no error; a failing tailnet is not an absent node", exists)
			}
			if err := c.Forget(ctx, "verge-machine-1"); err == nil {
				t.Fatal("Forget reported success against a failing tailnet")
			}
		})
	}
}

func TestCancelledContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a cancelled context reached the tailnet")
	})

	calls := []struct {
		name string
		call func() error
	}{
		{"bad/joined", func() error { _, err := c.Joined(cancelled, "verge-machine-1"); return err }},
		{"bad/node_exists", func() error { _, err := c.NodeExists(cancelled, "verge-machine-1"); return err }},
		{"bad/forget", func() error { return c.Forget(cancelled, "verge-machine-1") }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with a cancelled context = %v, want context.Canceled", tc.name, err)
			}
		})
	}
}

// TestEmptyNameIsRefused: an empty name would otherwise match the first device with
// no hostname, which is a bug that reads as a readiness success.
func TestEmptyNameIsRefused(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an empty name reached the tailnet")
	})
	if _, err := c.Joined(context.Background(), ""); err == nil {
		t.Fatal("Joined accepted an empty name")
	}
}

func TestTailnetPath(t *testing.T) {
	t.Run("happy/the_default_tailnet_is_a_dash", func(t *testing.T) {
		var got string
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			fmt.Fprint(w, devicesJSON())
		})
		if _, err := c.NodeExists(context.Background(), "m"); err != nil {
			t.Fatalf("NodeExists: %v", err)
		}
		if want := "/tailnet/-/devices"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("happy/a_named_tailnet_is_escaped_into_the_path", func(t *testing.T) {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			fmt.Fprint(w, devicesJSON())
		}))
		t.Cleanup(srv.Close)

		c := New(token, WithBaseURL(srv.URL), WithTailnet("example.com"))
		if _, err := c.NodeExists(context.Background(), "m"); err != nil {
			t.Fatalf("NodeExists: %v", err)
		}
		if want := "/tailnet/example.com/devices"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("bad/an_empty_tailnet_keeps_the_default", func(t *testing.T) {
		c := New(token, WithTailnet(""))
		if c.tailnet != defaultTailnet {
			t.Fatalf("tailnet = %q, want the default %q", c.tailnet, defaultTailnet)
		}
	})
}

// TestNotFoundIsRecognised keeps the 404 path from becoming a silent failure: the
// delete treats it as success, and nothing else does.
func TestNotFoundIsRecognised(t *testing.T) {
	if !isNotFound(errAPI{Status: http.StatusNotFound}) {
		t.Fatal("a 404 is not recognised")
	}
	if isNotFound(errAPI{Status: http.StatusForbidden}) {
		t.Fatal("a 403 is being treated as a 404")
	}
	if isNotFound(errors.New("tailnet: boom")) {
		t.Fatal("a plain error is being treated as a 404")
	}
}

// TestClientSatisfiesFirefly keeps the compile-time assertion honest in a test too,
// because the interface is what the package exists for.
func TestClientSatisfiesFirefly(t *testing.T) {
	var _ firefly.Network = New(token)
}
