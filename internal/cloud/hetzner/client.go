// Package hetzner is the real Cloud: the Hetzner Cloud API, reduced to the four
// calls firefly makes.
//
// Everything here is request/response against an injected *http.Client, so the
// unit tier runs against httptest and the contract tier swaps in a real token
// behind a build tag. No method retries: retrying is firefly's job, done from the
// document, because a retry loop inside a client is a retry loop with no record
// of what it was retrying.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// DefaultBaseURL is the public API root.
const DefaultBaseURL = "https://api.hetzner.cloud/v1"

// Client talks to the Hetzner Cloud API.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client somewhere else, which is what the httptest tier
// does and what a proxy or a recording would need.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient injects the transport. It is how a test asserts the timeout, and
// how the contract tier adds retry-free instrumentation without a second client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New returns a client for token.
//
// The token is a provider credential that can spend money, which is why it never
// reaches a machine: this client runs in the control plane and the only thing that
// crosses the boundary is a rendered cloud-init script.
func New(token string, opts ...Option) *Client {
	c := &Client{
		token:   token,
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

var _ firefly.Cloud = (*Client)(nil)

// --- API shapes ----------------------------------------------------------
//
// Narrow structs on purpose: a field firefly does not read is a field that will
// one day be relied on by accident, and the decode test below pins what is
// actually consumed.

type apiServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Created   string            `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errAPI turns a non-2xx response into an error that keeps the status and the
// provider's own message. A 401 and a 429 are the same to this package and
// different to a human, so both are preserved rather than flattened.
type errAPI struct {
	Status  int
	Code    string
	Message string
	Body    string
}

func (e errAPI) Error() string {
	switch {
	case e.Code != "" || e.Message != "":
		return fmt.Sprintf("hetzner: %d %s: %s", e.Status, e.Code, e.Message)
	case e.Body != "":
		return fmt.Sprintf("hetzner: %d: %s", e.Status, e.Body)
	default:
		return fmt.Sprintf("hetzner: %d", e.Status)
	}
}

// --- provider methods ----------------------------------------------------

// Create asks for a machine and returns its id.
func (c *Client) Create(ctx context.Context, spec firefly.Spec) (firefly.MachineID, error) {
	body := map[string]any{
		"name":      spec.Labels["verge-machine"],
		"image":     spec.Image,
		"labels":    spec.Labels,
		"user_data": string(spec.CloudInit),
	}
	if spec.Size != "" {
		body["server_type"] = spec.Size
	}
	if spec.Region != "" {
		body["location"] = spec.Region
	}
	// A name is required by the API and a name derived from the label keeps two
	// deployments distinguishable in the console. It is not identity: firefly
	// never reads it back.
	if body["name"] == "" {
		body["name"] = "verge"
	}

	var out struct {
		Server apiServer `json:"server"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", body, &out); err != nil {
		return "", err
	}
	return machineID(out.Server.ID), nil
}

// Describe reports one machine. A 404 is ErrNoSuchMachine and nothing else is.
func (c *Client) Describe(ctx context.Context, id firefly.MachineID) (firefly.CloudMachine, error) {
	n, err := serverID(id)
	if err != nil {
		return firefly.CloudMachine{}, err
	}

	var out struct {
		Server apiServer `json:"server"`
	}
	err = c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(n, 10), nil, &out)
	if isNotFound(err) {
		return firefly.CloudMachine{}, firefly.ErrNoSuchMachine
	}
	if err != nil {
		return firefly.CloudMachine{}, err
	}
	return convert(out.Server)
}

// List reports every machine carrying label, which is "key=value".
func (c *Client) List(ctx context.Context, label string) ([]firefly.CloudMachine, error) {
	path := "/servers"
	if label != "" {
		path += "?label_selector=" + url.QueryEscape(label)
	}

	var out struct {
		Servers []apiServer `json:"servers"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}

	machines := make([]firefly.CloudMachine, 0, len(out.Servers))
	for _, s := range out.Servers {
		m, err := convert(s)
		if err != nil {
			return nil, err
		}
		machines = append(machines, m)
	}
	return machines, nil
}

// Delete destroys a machine.
//
// A 404 is nil: the caller cannot tell an already-gone machine from a clean
// delete, and a second delete answering "gone" is not a failure.
func (c *Client) Delete(ctx context.Context, id firefly.MachineID) error {
	n, err := serverID(id)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(n, 10), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// --- plumbing ------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("hetzner: encoding the request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("hetzner: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hetzner: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("hetzner: reading the response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := errAPI{Status: resp.StatusCode}
		var payload apiError
		if json.Unmarshal(raw, &payload) == nil {
			e.Code = payload.Error.Code
			e.Message = payload.Error.Message
		}
		if e.Code == "" && e.Message == "" {
			e.Body = truncate(string(raw), 200)
		}
		return e
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hetzner: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// convert maps the provider's view onto firefly's.
//
// This is the only place the two vocabularies meet. An unrecognised status is an
// error rather than a guess: a provider that grows a status is a reason to
// upgrade, and guessing is how a machine that is off gets treated as running.
func convert(s apiServer) (firefly.CloudMachine, error) {
	status, err := convertStatus(s.Status)
	if err != nil {
		return firefly.CloudMachine{}, err
	}

	created, err := time.Parse(time.RFC3339, s.Created)
	if err != nil {
		return firefly.CloudMachine{}, fmt.Errorf("hetzner: server %d has created %q: %w", s.ID, s.Created, err)
	}

	return firefly.CloudMachine{
		ID:        machineID(s.ID),
		Status:    status,
		Labels:    s.Labels,
		PublicIP:  s.PublicNet.IPv4.IP,
		CreatedAt: created,
	}, nil
}

func convertStatus(status string) (firefly.ProviderStatus, error) {
	switch status {
	case "initializing":
		return firefly.ProviderCreated, nil
	case "starting", "migrating", "rebuilding":
		return firefly.ProviderStarting, nil
	case "running":
		return firefly.ProviderRunning, nil
	case "stopping", "off", "deleting":
		return firefly.ProviderStopping, nil
	default:
		return "", fmt.Errorf("hetzner: unknown server status %q", status)
	}
}

// machineID is how a provider id becomes firefly's opaque one. The prefix keeps a
// Hetzner id from being mistaken for some other provider's during a migration,
// and nothing else reads it.
func machineID(n int64) firefly.MachineID {
	return firefly.MachineID("hetzner:" + strconv.FormatInt(n, 10))
}

// serverID reverses machineID, and refuses anything else: passing a machine id
// from another provider to a Hetzner delete is a bug worth an error, not a 404.
func serverID(id firefly.MachineID) (int64, error) {
	s := string(id)
	const prefix = "hetzner:"
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return 0, fmt.Errorf("hetzner: %q is not a hetzner machine id", id)
	}
	n, err := strconv.ParseInt(s[len(prefix):], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("hetzner: %q is not a hetzner machine id: %w", id, err)
	}
	return n, nil
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
