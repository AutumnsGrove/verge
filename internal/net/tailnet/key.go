package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

// Enrolment keys.
//
// Network only observes and repairs, because joining happens inside the machine: the
// cloud-init script writes a key to the filesystem and the image's join step spends it.
// Something has to mint that key, and it is this package — the same credential, the same
// API, the same belief about what a machine may be trusted with.
//
// Three of the four capabilities are not configurable, and that is the decision:
//
//   - **ephemeral**, because deletion depends on it. An ephemeral node's record
//     disappears when the machine disconnects, so `NodeExists` usually resolves itself
//     and `Forget` is the repair for the times it does not (docs/firefly.md, "Readiness
//     is two facts"). A key that made persistent nodes would quietly turn a self-clearing
//     tailnet into one that accumulates records a sweep has to chase.
//   - **not reusable**, because a key that can enrol a second machine is a key that
//     escapes the machine it was written for.
//   - **preauthorized**, because the alternative is a device waiting for a human to
//     approve it, which from firefly's side is indistinguishable from a machine that
//     never joined — it would churn until `MaxLifetime` and call it a boot failure.
//
// The one thing that *is* configurable is the expiry, because only the caller knows how
// slow its boot path is; see MaxKeyTTL for the ceiling that keeps "short-lived" true.

// MaxDescription is the API's ceiling for a key's description, in bytes.
//
// It is a hard cap, not a preference: a description longer than this is a 400, and a 400
// here means no key and a machine that can never boot. Firefly's own names can reach 63
// characters, so a prefix plus a name can exceed it — see describedName.
const MaxDescription = 50

// MinKeyTTL is the shortest key this package will mint.
//
// The API takes a whole number of seconds, and **zero means no expiry at all** — not
// "expires immediately". A sub-second TTL would therefore truncate to a permanent key,
// which inverts the one guarantee this file exists to make, so it is refused rather than
// rounded: a caller asking for half a second has asked for something that cannot be
// honoured, and rounding it up would hand back a key that dies before a machine can boot
// while looking like success.
const MinKeyTTL = time.Second

// MaxKeyTTL bounds how long an enrolment key may live.
//
// A key is valid from mint to join, and the window between them is one `Create` plus one
// boot — `BootDeadline` by default. An hour is generous against that and still short
// enough that a key sitting in a queue behind a wedged provisioning call becomes useless
// rather than dangerous. The ceiling exists because "short-lived" is a claim this
// package makes in prose about a value a caller chooses; without it the claim is a wish.
const MaxKeyTTL = time.Hour

// ErrKeyRequest means the request for an enrolment key cannot be made.
var ErrKeyRequest = errors.New("tailnet: invalid enrolment key request")

// Key is a minted enrolment key.
//
// The plaintext is returned once, by the API, and is never recoverable afterwards — so
// nothing here logs it, stores it, or puts it in an error. It exists to be rendered into
// a boot script and forgotten, which is what the field comment on AuthKey in
// `cloudinit.Spec` means by a credential that grants membership and nothing else.
type Key struct {
	// Value is the key itself, as a machine passes it to the mesh agent.
	Value string

	// Expires is when it stops working, as reported by the API. It is information for
	// the caller's logs; nothing here decides anything with it.
	Expires time.Time

	// Description is what was asked for, echoed back so a log line can say which
	// machine a key was minted for without keeping the key.
	Description string
}

// describedName builds the description an operator sees in the admin console next to the
// device a key created, within the API's byte limit.
//
// The *tail* of the name is what survives truncation, because that is the part that tells
// two machines of one deployment apart: names are minted as "<label>-<counter>", so a long
// label truncated from the front keeps the counter and a long label truncated from the end
// would not.
func describedName(name string) string {
	const prefix = "verge "
	budget := MaxDescription - len(prefix)
	if len(name) <= budget {
		return prefix + name
	}

	// "…" is three bytes, so it comes out of the budget.
	tail := name[len(name)-(budget-3):]
	// The budget is in bytes and the name is not necessarily ASCII, so a byte offset can
	// land in the middle of a rune. That would be invalid UTF-8, and encoding/json
	// replaces each broken byte with U+FFFD — three bytes for one — which grows the
	// description back over the limit this function exists to stay under. Backing up to
	// the next rune boundary keeps the arithmetic exact and the name readable; shortening
	// it by up to three bytes costs nothing.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return prefix + "…" + tail
}

// MintKey requests a single-use ephemeral enrolment key for the named machine.
//
// The description is what an operator sees in the tailnet's admin console next to the
// device a key created, so it names the machine rather than saying "verge". ttl must be
// positive and at most MaxKeyTTL.
func (c *Client) MintKey(ctx context.Context, name string, ttl time.Duration) (Key, error) {
	switch {
	case name == "":
		return Key{}, fmt.Errorf("%w: no machine name", ErrKeyRequest)
	case !utf8.ValidString(name):
		// encoding/json rewrites invalid UTF-8 as U+FFFD, which for a description is a
		// mangled name and up to three bytes per broken byte — enough to break the length
		// budget above, and enough to make a log line unreadable. A name that is not text
		// is not a hostname either.
		return Key{}, fmt.Errorf("%w: the machine name is not valid UTF-8", ErrKeyRequest)
	case ttl < MinKeyTTL:
		// Not merely "too short": the wire value is whole seconds and zero means never
		// expires, so this is the difference between a key that lapses in a minute and
		// one that never does.
		return Key{}, fmt.Errorf("%w: ttl %s is under %s, and a truncated expiry of zero means no expiry", ErrKeyRequest, ttl, MinKeyTTL)
	case ttl > MaxKeyTTL:
		return Key{}, fmt.Errorf("%w: ttl %s is longer than the %s ceiling", ErrKeyRequest, ttl, MaxKeyTTL)
	}

	description := describedName(name)
	body := map[string]any{
		"description":   description,
		"expirySeconds": int(ttl.Seconds()),
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      false,
					"ephemeral":     true,
					"preauthorized": true,
				},
			},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Key{}, fmt.Errorf("tailnet: encoding the key request: %w", err)
	}

	var out struct {
		Key     string `json:"key"`
		Expires string `json:"expires"`
	}
	if err := c.do(ctx, http.MethodPost, "/tailnet/"+url.PathEscape(c.tailnet)+"/keys",
		bytes.NewReader(raw), &out); err != nil {
		return Key{}, err
	}

	// A mint that returns no key is not a key. Handing an empty string to a renderer
	// would produce a boot script that fails `cloudinit.Render`'s own check — but only
	// after a machine and a key had been spent, so it is an error here.
	if out.Key == "" {
		return Key{}, errors.New("tailnet: the API returned a key without a value")
	}

	key := Key{Value: out.Key, Description: description}
	if out.Expires != "" {
		// Parsed but not required: the API reports it for logs, and a format this
		// client does not recognise is not a reason to throw away a working key.
		if expires, err := time.Parse(time.RFC3339, out.Expires); err == nil {
			key.Expires = expires
		}
	}
	return key, nil
}
