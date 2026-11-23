// Package fake is the one fake cloud, shared by every test that needs a
// provider. docs/testing.md calls for one fake rather than a stub per test file,
// because divergent fakes are how a suite starts lying.
//
// It models the provider as its own state machine:
//
//	absent → created → starting → running → stopping → deleted
//
// Every duration comes from the fake's own clock, which a test advances. No
// method sleeps and no method reads the wall clock, so the integration tier stays
// instant.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// Cloud is a provider that does exactly what a test tells it to.
//
// It is safe for concurrent use, because the reconcile loop and the surface that
// answers a phone both reach it and "probably single-threaded" is not a property
// worth relying on.
type Cloud struct {
	mu   sync.Mutex
	now  time.Time
	seq  int
	mach map[firefly.MachineID]*machine

	// Timing, in simulated time.
	bootDelay   time.Duration
	deleteDelay time.Duration

	// Faults. Each is a named behaviour from docs/testing.md and each has at
	// least one sad/ case that asserts what firefly does about it.
	failCreate       error
	failList         error
	stallBoot        bool
	createIsSlow     time.Duration
	deleteRefuses    int
	deleteReturns404 bool
	listDrops        map[firefly.MachineID]struct{}

	// Counts, so a test can assert that the loop does not call the provider more
	// often than the design says it does.
	creates   int
	describes int
	lists     int
	deletes   int

	// specs is every request Create has been handed, in order. It exists so a test
	// can compare the name the provider was told with the name the network is later
	// asked about: the fake derives nodes from its own records, so it cannot notice
	// that the two disagree, and nothing else in the design joins them.
	specs []firefly.Spec
}

type machine struct {
	id        firefly.MachineID
	spec      firefly.Spec
	name      string
	createdAt time.Time
	readyAt   time.Time // zero means never: the boot never finishes
	deleteAt  time.Time // zero means no delete has been requested
	// goneAt is when the machine stopped existing; zero means it still does. It is
	// the only record of death, because a separate flag could disagree with it --
	// which is exactly how a 404-reaped machine grew a node record once.
	goneAt   time.Time
	publicIP string
}

// New returns a fake positioned at now, with a 90-second boot and a 5-second
// delete. Tests that care about either set their own.
func New(now time.Time) *Cloud {
	return &Cloud{
		now:         now,
		mach:        make(map[firefly.MachineID]*machine),
		bootDelay:   90 * time.Second,
		deleteDelay: 5 * time.Second,
		listDrops:   make(map[firefly.MachineID]struct{}),
	}
}

// The fake must satisfy the interface firefly depends on.
var _ firefly.Cloud = (*Cloud)(nil)

// --- clock ---------------------------------------------------------------

// Now reports the fake's clock. It is not the wall clock.
func (c *Cloud) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake's clock forward. A four-minute boot is instant unless a
// test advances time, which is the whole reason the fake owns a clock.
func (c *Cloud) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Boot sets how long a machine spends starting. Zero means it is running the
// instant it is created, which is useful for a test that is not about booting.
func (c *Cloud) Boot(d time.Duration) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bootDelay = d
	return c
}

// DeleteAfter sets how long a delete takes to complete once accepted.
func (c *Cloud) DeleteAfter(d time.Duration) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteDelay = d
	return c
}

// --- faults --------------------------------------------------------------

// FailCreate makes every create return err and create no machine, which is what
// quota exhaustion, an expired token, and a malformed request all look like from
// here.
func (c *Cloud) FailCreate(err error) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failCreate = err
	return c
}

// FailList makes every listing fail, which is how a tick meets an unreachable
// provider rather than an empty one.
func (c *Cloud) FailList(err error) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failList = err
	return c
}

// StallBoot makes a machine that never finishes cloud-init: it stays created and
// starts forever, and never becomes running.
func (c *Cloud) StallBoot() *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stallBoot = true
	return c
}

// CreateIsSlow makes a create consume d of simulated time before returning.
//
// It does not sleep, because the fast tier forbids that and because a sleep is
// the wrong shape for a deterministic test. The caller's own timeout is asserted
// by passing a context that is already done, which exercises the same path in
// firefly without waiting for anything.
func (c *Cloud) CreateIsSlow(d time.Duration) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createIsSlow = d
	return c
}

// DeleteRefuses makes the next n deletes fail before one succeeds.
func (c *Cloud) DeleteRefuses(n int) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteRefuses = n
	return c
}

// DeleteReturns404 makes delete report success for any id, including one the
// provider never had. It models the common real case — a delete submitted twice,
// a machine reaped by the provider, an operator using the console — and firefly
// must treat it as a clean delete rather than an error.
func (c *Cloud) DeleteReturns404() *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteReturns404 = true
	return c
}

// ListDrops makes List omit id even though the machine exists. This is eventual
// consistency, and it is the fault that punishes deriving absence from a
// listing.
func (c *Cloud) ListDrops(id firefly.MachineID) *Cloud {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listDrops[id] = struct{}{}
	return c
}

// --- provider ------------------------------------------------------------

// Create makes a machine and returns its id.
func (c *Cloud) Create(ctx context.Context, spec firefly.Spec) (firefly.MachineID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.failCreate != nil {
		return "", c.failCreate
	}
	if c.createIsSlow > 0 {
		c.now = c.now.Add(c.createIsSlow)
	}

	c.seq++
	id := firefly.MachineID(fmt.Sprintf("fake-%03d", c.seq))

	// A name is what the network knows the machine by, and the provider requires it
	// to be unique within the project: a real create with a taken name fails with
	// uniqueness_error. The fake models that rather than accepting it, because two
	// machines sharing a name share one node record. A nameless request gets one
	// minted here, which is what the real client does when a caller skips it.
	name := spec.Name
	if name == "" {
		name = fmt.Sprintf("fake-name-%03d", c.seq)
	}
	for _, other := range c.mach {
		if other.name == name && !c.expired(other) {
			return "", fmt.Errorf("fake: the provider refused the create: name %q is taken", name)
		}
	}
	spec.Name = name

	m := &machine{
		id:        id,
		name:      name,
		spec:      copySpec(spec),
		createdAt: c.now,
		publicIP:  fmt.Sprintf("10.0.0.%d", c.seq),
	}
	if !c.stallBoot {
		m.readyAt = c.now.Add(c.bootDelay)
	}
	c.mach[id] = m
	c.specs = append(c.specs, copySpec(spec))
	return id, nil
}

// Describe reports one machine, completing a pending delete if its time is up.
func (c *Cloud) Describe(ctx context.Context, id firefly.MachineID) (firefly.CloudMachine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.describes++

	if err := ctx.Err(); err != nil {
		return firefly.CloudMachine{}, err
	}
	m, ok := c.mach[id]
	if !ok {
		return firefly.CloudMachine{}, firefly.ErrNoSuchMachine
	}
	if c.expired(m) {
		return firefly.CloudMachine{}, firefly.ErrNoSuchMachine
	}
	return m.view(c.status(m)), nil
}

// List reports every machine carrying label, which is "key=value".
//
// Dropped ids are omitted without being forgotten, which is the whole point of
// the fault: the fake still knows the machine exists.
func (c *Cloud) List(ctx context.Context, label string) ([]firefly.CloudMachine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.failList != nil {
		return nil, c.failList
	}

	var out []firefly.CloudMachine
	for _, m := range c.mach {
		if c.expired(m) {
			continue
		}
		if _, dropped := c.listDrops[m.id]; dropped {
			continue
		}
		if label != "" && !hasLabel(m.spec.Labels, label) {
			continue
		}
		out = append(out, m.view(c.status(m)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Delete asks the provider to destroy a machine.
//
// It is idempotent and a second call is not an error: the caller cannot tell a
// repeat delete from a clean one, and should not try.
func (c *Cloud) Delete(ctx context.Context, id firefly.MachineID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++

	if err := ctx.Err(); err != nil {
		return err
	}

	if c.deleteReturns404 {
		// The provider says it never had this machine, so it is gone as of now.
		// goneAt is what lets the network half tell "reaped before it could join"
		// from "joined and then reaped". An instant expresses both; the flag this
		// path used to set expressed neither.
		if m, ok := c.mach[id]; ok && m.goneAt.IsZero() {
			m.goneAt = c.now
		}
		return nil
	}

	m, ok := c.mach[id]
	if !ok {
		return firefly.ErrNoSuchMachine
	}
	if c.expired(m) {
		return nil
	}
	if c.deleteRefuses > 0 {
		c.deleteRefuses--
		return fmt.Errorf("fake: the provider refused the delete (%d refusals remaining)", c.deleteRefuses)
	}
	if !m.deleteAt.IsZero() {
		return nil
	}
	m.deleteAt = c.now.Add(c.deleteDelay)
	m.goneAt = m.deleteAt
	return nil
}

// --- counts --------------------------------------------------------------

// Calls reports how many times each provider method has been called.
func (c *Cloud) Calls() (creates, describes, lists, deletes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates, c.describes, c.lists, c.deletes
}

// --- internals -----------------------------------------------------------

// status is the provider's view, derived from the clock rather than stored, so
// that advancing time is the only way to change it.
func (c *Cloud) status(m *machine) firefly.ProviderStatus {
	switch {
	case !m.deleteAt.IsZero():
		return firefly.ProviderStopping
	case m.readyAt.IsZero():
		// A machine that never finishes cloud-init reports starting forever.
		return firefly.ProviderStarting
	case c.now.Equal(m.createdAt):
		return firefly.ProviderCreated
	case c.now.Before(m.readyAt):
		return firefly.ProviderStarting
	default:
		return firefly.ProviderRunning
	}
}

// expired reports whether the machine is gone: the instant has passed.
//
// It mutates nothing now that goneAt is the only record of death, which matters
// more than it looks -- a read that also wrote was how the old flag came to
// disagree with the timestamp.
func (c *Cloud) expired(m *machine) bool {
	return !m.goneAt.IsZero() && !c.now.Before(m.goneAt)
}

func (m *machine) view(status firefly.ProviderStatus) firefly.CloudMachine {
	return firefly.CloudMachine{
		ID:        m.id,
		Name:      m.name,
		Status:    status,
		Labels:    copyLabels(m.spec.Labels),
		PublicIP:  m.publicIP,
		CreatedAt: m.createdAt,
	}
}

func copySpec(s firefly.Spec) firefly.Spec {
	s.CloudInit = append([]byte(nil), s.CloudInit...)
	s.Labels = copyLabels(s.Labels)
	return s
}

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// byName returns the machine the network should reason about for a name.
//
// It prefers a live machine and falls back to the most recent dead one, because
// the two callers want opposite halves of the same fact: joining is only ever true
// for a machine that still exists, while a node record has to remain observable
// after its machine is gone -- that lingering record is the whole reason Forget
// exists. A name cannot be reused by a live machine while a live one holds it
// (Create refuses that), so preferring the live one is unambiguous.
//
// It is called with the cloud's lock held, by the network half, which is why there
// is no lock of its own.
func (c *Cloud) byName(name string) (*machine, bool) {
	var dead *machine
	for _, m := range c.mach {
		if m.name != name {
			continue
		}
		if !c.expired(m) {
			return m, true
		}
		if dead == nil || m.createdAt.After(dead.createdAt) {
			dead = m
		}
	}
	if dead != nil {
		return dead, true
	}
	return nil, false
}

// Name reports the name the provider gave a machine, whether or not it still
// exists.
//
// It is not part of the Cloud interface: firefly learns a machine's name from its
// own record, and from the listing when it adopts one, never by looking it up
// again. It is here because a test that holds only an id cannot ask the network
// anything -- which is the whole point of the rekey.
func (c *Cloud) Name(id firefly.MachineID) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.mach[id]
	if !ok {
		return "", false
	}
	return m.name, true
}

// Specs reports every request Create has been handed, in order.
func (c *Cloud) Specs() []firefly.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]firefly.Spec, len(c.specs))
	for i, s := range c.specs {
		out[i] = copySpec(s)
	}
	return out
}

// hasLabel reports whether labels carry "key=value".
func hasLabel(labels map[string]string, label string) bool {
	key, want, ok := strings.Cut(label, "=")
	if !ok {
		return false
	}
	got, present := labels[key]
	return present && got == want
}
