package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/cloudinit"
)

// mint answers one key request and reports what it was asked.
type mintRequest struct {
	path, method, auth, contentType string
	body                            map[string]any
}

func newMinter(t *testing.T, status int, response string) (*Client, *mintRequest) {
	t.Helper()
	got := &mintRequest{}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.path, got.method = r.URL.Path, r.Method
		got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got.body); err != nil {
			t.Errorf("the request body is not JSON: %v", err)
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
		}
		io.WriteString(w, response)
	})
	return c, got
}

// capabilities reaches into the nested body a key request has.
func capabilities(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	devices, ok := body["capabilities"].(map[string]any)["devices"].(map[string]any)
	if !ok {
		t.Fatalf("the request has no capabilities.devices: %v", body)
	}
	create, ok := devices["create"].(map[string]any)
	if !ok {
		t.Fatalf("the request has no capabilities.devices.create: %v", body)
	}
	return create
}

func TestMintKey(t *testing.T) {
	ctx := context.Background()

	t.Run("happy/mints_a_single_use_ephemeral_key_for_one_machine", func(t *testing.T) {
		c, req := newMinter(t, http.StatusOK, `{
			"id": "k1",
			"key": "tskey-auth-k1-abc",
			"created": "2026-11-22T12:00:00Z",
			"expires": "2026-11-22T12:15:00Z"
		}`)

		key, err := c.MintKey(ctx, "verge-machine-1", 15*time.Minute)
		if err != nil {
			t.Fatalf("MintKey: %v", err)
		}

		if req.method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.method)
		}
		if want := "/tailnet/-/keys"; req.path != want {
			t.Errorf("path = %q, want %q", req.path, want)
		}
		if req.auth != "Bearer "+token {
			t.Errorf("Authorization = %q", req.auth)
		}
		if req.contentType != "application/json" {
			t.Errorf("Content-Type = %q", req.contentType)
		}

		// The three capabilities are the design's promises, so they are asserted as
		// values rather than assumed: turning any one of them off is a change to how
		// deletion works, not a preference.
		create := capabilities(t, req.body)
		if create["ephemeral"] != true {
			t.Error("ephemeral is not true: node records would outlive their machines by default")
		}
		if create["reusable"] != false {
			t.Error("reusable is not false: one key could enrol more than one machine")
		}
		if create["preauthorized"] != true {
			t.Error("preauthorized is not true: a device would wait for a human and look like a failed boot")
		}
		if got := req.body["expirySeconds"]; got != float64(900) {
			t.Errorf("expirySeconds = %v, want 900", got)
		}
		// The description is what an operator reads in the console next to the device.
		if got, _ := req.body["description"].(string); !strings.Contains(got, "verge-machine-1") {
			t.Errorf("description = %q, want it to name the machine", got)
		}

		if key.Value != "tskey-auth-k1-abc" {
			t.Errorf("Value = %q", key.Value)
		}
		if want := time.Date(2026, 11, 22, 12, 15, 0, 0, time.UTC); !key.Expires.Equal(want) {
			t.Errorf("Expires = %s, want %s", key.Expires, want)
		}
	})

	// The API reports an expiry the caller only logs with; a format this client does not
	// recognise must not throw away a key that works.
	t.Run("happy/an_unparseable_expiry_costs_the_log_line_only", func(t *testing.T) {
		c, _ := newMinter(t, http.StatusOK, `{"key":"tskey-auth-k1-abc","expires":"soon"}`)
		key, err := c.MintKey(ctx, "verge-machine-1", time.Minute)
		if err != nil {
			t.Fatalf("MintKey: %v", err)
		}
		if key.Value == "" || !key.Expires.IsZero() {
			t.Fatalf("key = %+v, want the value and no expiry", key)
		}
	})

	t.Run("bad/an_api_that_returns_no_key_is_an_error", func(t *testing.T) {
		// Rendering an empty key would spend a machine and fail in cloudinit instead.
		c, _ := newMinter(t, http.StatusOK, `{"id":"k1"}`)
		if _, err := c.MintKey(ctx, "verge-machine-1", time.Minute); err == nil {
			t.Fatal("MintKey accepted a response with no key")
		}
	})

	t.Run("sad/a_refused_request", func(t *testing.T) {
		c, _ := newMinter(t, http.StatusForbidden, `{"message":"key creation not allowed"}`)
		_, err := c.MintKey(ctx, "verge-machine-1", time.Minute)
		if err == nil {
			t.Fatal("MintKey reported success for a refused request")
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("the refusal lost its message: %v", err)
		}
	})

	t.Run("bad/a_tailnet_that_answers_garbage", func(t *testing.T) {
		c, _ := newMinter(t, http.StatusOK, `{"key":`)
		if _, err := c.MintKey(ctx, "verge-machine-1", time.Minute); err == nil {
			t.Fatal("MintKey accepted malformed JSON")
		}
	})

	cases := []struct {
		name string
		ttl  time.Duration
	}{
		{"bad/zero_ttl", 0},
		{"bad/negative_ttl", -time.Minute},
		{"bad/a_key_that_would_outlive_its_purpose", MaxKeyTTL + time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newMinter(t, http.StatusOK, `{}`)
			if _, err := c.MintKey(ctx, "verge-machine-1", tc.ttl); !errors.Is(err, ErrKeyRequest) {
				t.Fatalf("MintKey(ttl %s) = %v, want ErrKeyRequest", tc.ttl, err)
			}
		})
	}

	t.Run("bad/an_empty_machine_name", func(t *testing.T) {
		c, _ := newMinter(t, http.StatusOK, `{}`)
		if _, err := c.MintKey(ctx, "", time.Minute); !errors.Is(err, ErrKeyRequest) {
			t.Fatalf("MintKey with no name = %v, want ErrKeyRequest", err)
		}
	})

	t.Run("bad/a_cancelled_context", func(t *testing.T) {
		c, _ := newMinter(t, http.StatusOK, `{}`)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := c.MintKey(cancelled, "verge-machine-1", time.Minute); !errors.Is(err, context.Canceled) {
			t.Fatalf("MintKey = %v, want context.Canceled", err)
		}
	})
}

// TestMintKeyFitsTheRenderer is the join between the two packages: what this mints is
// what a boot script carries. Neither package can check the other on its own — the
// renderer refuses an empty key, and a mint that returned one would only be discovered
// when a machine failed to boot.
func TestMintKeyFitsTheRenderer(t *testing.T) {
	c, _ := newMinter(t, http.StatusOK, `{"key":"tskey-auth-k1abcdef"}`)
	key, err := c.MintKey(context.Background(), "verge-machine-1", time.Minute)
	if err != nil {
		t.Fatalf("MintKey: %v", err)
	}

	script, err := cloudinit.Render(cloudinit.Spec{
		Hostname: "verge-machine-1",
		MeshAddr: "https://control.example.ts.net",
		AuthKey:  key.Value,
	})
	if err != nil {
		t.Fatalf("the minted key does not render: %v", err)
	}
	if !strings.Contains(string(script), key.Value) {
		t.Fatal("the boot script does not carry the key")
	}
}
