// Package tailnet is the real Network: a Tailscale tailnet, reduced to the three
// observations a lifecycle makes.
//
// There is no join here. A machine joins by running a mesh agent with a single-use
// ephemeral enrolment key, and the key travels inside the rendered cloud-init —
// firefly.Spec carries the script, not the key, because a second copy of a secret
// would be a second source of truth for it. So this client only ever asks and
// repairs: is the node there, is it usable, make it go away.
//
// One assumption is load-bearing and is not enforced here: a node must join under
// the name firefly minted for its machine. This package matches a device by its
// hostname (falling back to the first label of its MagicDNS name), because that is
// the only name a tailnet knows — so a machine that joins under the image's default
// hostname is a machine nothing will ever find, and it will sit `PROVISIONING` until
// the boot deadline destroys it. The requirement is the *renderer's*: the script must
// set the hostname to firefly.Spec.Name, and the enrolment key must be
// `preauthorized` or the device waits for a human. Both belong to that PR, and this
// is the sentence to check it against.
//
// Everything here is request/response against an injected *http.Client, so the unit
// tier runs against httptest and the contract tier swaps in a real API token behind
// a build tag. No method retries: retrying is firefly's job, done from the document,
// because a retry loop inside a client is a retry loop with no record of what it was
// retrying.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// DefaultBaseURL is the public API root.
const DefaultBaseURL = "https://api.tailscale.com/api/v2"

// defaultTailnet asks the API for the default tailnet of whichever token is in use.
//
// The API documents "-" for exactly this, and it is the option worth defaulting to:
// a tailnet ID is a deployment detail that would otherwise have to be pasted into
// configuration, and a token already belongs to one.
const defaultTailnet = "-"

// Client talks to the Tailscale API.
type Client struct {
	token   string
	tailnet string
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client somewhere else, which is what the httptest tier does
// and what a recording or a self-hosted control server would need.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithTailnet names the tailnet to ask about. Empty means the default tailnet of the
// token, which is what an operator running one tailnet wants.
func WithTailnet(name string) Option {
	return func(c *Client) {
		if name != "" {
			c.tailnet = name
		}
	}
}

// WithHTTPClient injects the transport. It is how the contract tier adds
// instrumentation without a second client, and how a caller running inside its own
// deadline shortens the timeout below the default.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New returns a client for token.
//
// The token is a control-plane credential: it can read the tailnet and delete
// devices from it. Nothing in this package puts it anywhere near a machine, and the
// thing a machine does get — an ephemeral enrolment key — grants membership and
// nothing else. That distinction is the same one the whole design rests on.
func New(token string, opts ...Option) *Client {
	c := &Client{
		token:   token,
		tailnet: defaultTailnet,
		baseURL: DefaultBaseURL,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ firefly.Network = (*Client)(nil)

// --- API shapes ----------------------------------------------------------
//
// Narrow on purpose, and narrower still than the API's own schema: a field nothing
// reads is a field that will one day be relied on by accident. The five fields here
// are the ones that decide the two answers.

type apiDevice struct {
	// ID is the legacy numeric identifier and NodeID the preferred one. Both are
	// accepted by the delete endpoint, and NodeID is preferred there for the same
	// reason it is preferred here.
	ID     string `json:"id"`
	NodeID string `json:"nodeId"`

	// Name is the MagicDNS name, "host.tailnet.ts.net." — fully qualified, with a
	// trailing dot. Hostname is what the machine reported at join, which is the name
	// a provider's server name becomes.
	Name     string `json:"name"`
	Hostname string `json:"hostname"`

	// Authorized and ConnectedToControl are what make a registered node a *usable*
	// one. They are the tailnet's own halves of readiness: a device awaiting
	// approval, or whose agent is not talking to the control plane, exists and is not
	// joined.
	Authorized         bool `json:"authorized"`
	ConnectedToControl bool `json:"connectedToControl"`
}

// errAPI turns a non-2xx response into an error that keeps the status and the
// provider's own message. A 401 and a 429 are the same to this package and different
// to a human, so both are preserved rather than flattened.
type errAPI struct {
	Status  int
	Message string
	Body    string
}

func (e errAPI) Error() string {
	switch {
	case e.Message != "":
		return fmt.Sprintf("tailnet: %d: %s", e.Status, e.Message)
	case e.Body != "":
		return fmt.Sprintf("tailnet: %d: %s", e.Status, e.Body)
	default:
		return fmt.Sprintf("tailnet: %d", e.Status)
	}
}

// ErrAmbiguousNode means more than one device answers to the requested name.
//
// It is an error rather than a guess because every guess is wrong in a way that
// costs: picking one device for Joined can mark a machine ready that is not, and
// picking one for Forget deletes a node that belongs to a machine firefly did not
// create. Two devices with one hostname means two deployments minted the same name
// into one tailnet, which is a configuration a human has to look at.
var ErrAmbiguousNode = errors.New("tailnet: more than one device has that name")

// --- firefly.Network -----------------------------------------------------

// Joined reports whether a node exists for name and is usable right now.
//
// Usable is the tailnet's answer to the question, which is two of its own facts: the
// device is authorized, and its agent is connected to the control plane. It is
// deliberately *not* a question about the provider — a network cannot see whether a
// server is running — and the caller already asks the provider separately. Readiness
// is the two answers together, which is why neither is trusted alone.
func (c *Client) Joined(ctx context.Context, name string) (bool, error) {
	d, found, err := c.find(ctx, name)
	if err != nil || !found {
		return false, err
	}
	return d.Authorized && d.ConnectedToControl, nil
}

// NodeExists reports whether a node record exists for name, whether or not the
// device is usable.
//
// This is the half of deletion that a machine cannot resolve for itself: a provider
// can say a machine is gone while the tailnet still lists its device, which is how a
// tailnet accumulates stale nodes quietly.
func (c *Client) NodeExists(ctx context.Context, name string) (bool, error) {
	_, found, err := c.find(ctx, name)
	return found, err
}

// Forget removes the device record for name.
//
// The endpoint is keyed on a device id rather than a name, so this lists first. A
// name with no device is not an error: the caller cannot tell a node that was reaped
// by the tailnet from one it removed itself, and an ephemeral key means the ordinary
// case is that the record has already gone.
func (c *Client) Forget(ctx context.Context, name string) error {
	d, found, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	id := d.NodeID
	if id == "" {
		id = d.ID
	}
	if id == "" {
		return fmt.Errorf("tailnet: the device named %q has no id to delete", name)
	}

	err = c.do(ctx, http.MethodDelete, "/device/"+url.PathEscape(id), nil)
	if err != nil && isNotFound(err) {
		// Raced with an ephemeral node disappearing on its own. Same outcome.
		return nil
	}
	return err
}

// --- internals -----------------------------------------------------------

// find returns the device that answers to name, listing once.
//
// Every method lists rather than caching, which is a cost rather than an oversight:
// the tailnet API has no pagination and no lookup by name, so one listing per
// question is the honest shape. A cache would make the answer depend on whether
// something happened to ask earlier — the exact bug the fake's node derivation was
// rewritten to remove — and it would have to decide how long a cached fact about a
// machine that may have joined or died is still true.
func (c *Client) find(ctx context.Context, name string) (apiDevice, bool, error) {
	if name == "" {
		return apiDevice{}, false, errors.New("tailnet: asked about an empty name")
	}

	var out struct {
		Devices []apiDevice `json:"devices"`
	}
	if err := c.do(ctx, http.MethodGet, "/tailnet/"+url.PathEscape(c.tailnet)+"/devices", &out); err != nil {
		return apiDevice{}, false, err
	}

	var found []apiDevice
	for _, d := range out.Devices {
		if strings.EqualFold(deviceName(d), name) {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return apiDevice{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return apiDevice{}, false, fmt.Errorf("%w: %q is claimed by devices %s and %s",
			ErrAmbiguousNode, name, deviceID(found[0]), deviceID(found[1]))
	}
}

// deviceName is what a device answers to.
//
// The hostname is the name the machine reported when it joined, which is the name the
// provider gave the server, which is the name firefly minted. The MagicDNS name is the
// fallback: it is the hostname plus the tailnet's domain, so a device that reported no
// hostname still has a first label worth matching.
func deviceName(d apiDevice) string {
	if d.Hostname != "" {
		return d.Hostname
	}
	name, _, _ := strings.Cut(d.Name, ".")
	return name
}

func deviceID(d apiDevice) string {
	if d.NodeID != "" {
		return d.NodeID
	}
	return d.ID
}

// --- plumbing ------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("tailnet: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tailnet: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("tailnet: reading the response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := errAPI{Status: resp.StatusCode, Body: truncate(string(raw), 200)}
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
			e.Message = payload.Message
		}
		return e
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("tailnet: decoding %s %s: %w", method, path, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var e errAPI
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
