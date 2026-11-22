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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// DefaultBaseURL is the public API root.
const DefaultBaseURL = "https://api.hetzner.cloud/v1"

const (
	// listPageSize is the API maximum, chosen so a normal deployment needs one
	// page and the loop is a formality.
	listPageSize = 50

	// maxListPages bounds the walk. A next_page that never clears would otherwise
	// spin forever against a provider that is answering honestly.
	maxListPages = 100
)

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
	name, err := serverName(spec.Name)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"name":      name,
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

	// The listing is paginated, and the default page size is smaller than a
	// deployment can get to. Returning the first page would make the orphan sweep
	// silently blind to the rest -- the same shape of failure as ListDrops, arrived
	// at through pagination instead of eventual consistency, and just as quiet.
	var machines []firefly.CloudMachine
	for page := 1; ; page++ {
		pagePath := path
		sep := "?"
		if strings.Contains(pagePath, "?") {
			sep = "&"
		}
		pagePath += sep + "per_page=" + strconv.Itoa(listPageSize) + "&page=" + strconv.Itoa(page)

		var out struct {
			Servers []apiServer `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage any `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, pagePath, nil, &out); err != nil {
			return nil, err
		}

		for _, s := range out.Servers {
			m, err := convert(s)
			if err != nil {
				return nil, err
			}
			machines = append(machines, m)
		}

		// next_page is null on the last page and a number otherwise. Treating it
		// as `any` is what makes both work without a custom unmarshaler.
		if out.Meta.Pagination.NextPage == nil {
			return machines, nil
		}
		if page > maxListPages {
			return nil, fmt.Errorf("hetzner: listing exceeded %d pages", maxListPages)
		}
	}
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

// serverName returns the name to give a machine.
//
// The API requires a name, requires it to be a valid hostname, and requires it to
// be *unique within the project*. A constant name therefore does not merely look
// untidy: it means the second machine in a project is rejected with
// uniqueness_error, and a caller can manage exactly one machine forever. Firefly
// never reads the name back -- identity is the provider id -- so uniqueness is the
// only property that matters, and a random suffix is the cheapest way to get it.
func serverName(requested string) (string, error) {
	if requested != "" {
		if !validHostname(requested) {
			return "", fmt.Errorf("hetzner: %q is not a valid server name (lowercase letters, digits, and hyphens)", requested)
		}
		return requested, nil
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("hetzner: generating a server name: %w", err)
	}
	return "verge-" + hex.EncodeToString(b[:]), nil
}

// validHostname checks the subset of RFC 1123 the API accepts. It is a guard, not
// a validator: a name that passes here and is rejected by the API is still an
// error the caller sees, which is the right way round.
func validHostname(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(s)-1:
		default:
			return false
		}
	}
	return true
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
