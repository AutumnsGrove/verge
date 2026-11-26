// Package syncer is the real firefly.Syncer: a machine's workspace, saved as one
// tarball in object storage.
//
// # The judgement calls this package makes
//
// firefly decides *when* a save must have happened; this decides what "saved"
// means, and the shape is Autumn's call: the machine tars its own workspace and
// the control plane ships those bytes to R2, keyed by machine id. A git bundle was
// rejected because it loses untracked files, and the transcript variant is
// deferred rather than refused.
//
// Three things are this package's own judgement, written down because the spec did
// not settle them:
//
//  1. **The transport is a boundary, not an implementation.** Fetching from a
//     machine that is about to be destroyed needs an agent on the machine and a
//     way through the tailnet; neither exists. Fetcher names the one method this
//     package needs, so the loop can be built and tested before the transport is.
//
//  2. **The workspace is spooled to a real file.** A spool is a second write of
//     something that may be gigabytes, and the alternative -- buffering in memory,
//     or handing the sink a stream of unknown length -- is worse: S3 wants a
//     ContentLength, and a seekable body is what lets the SDK retry without the
//     source being re-read from a machine that may already be gone by then.
//
//  3. **An empty fetch is nothing to save, and that is success.** firefly's
//     contract says nil when there was nothing to save, which is the ordinary case
//     for a machine that never became ready. Every other failure is returned: a
//     machine that cannot be reached is not a machine with nothing on it, and
//     treating it as one would delete the workspace this package exists to keep.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// MaxWorkspaceBytes bounds what one save will spool.
//
// A workspace is a checkout plus whatever an agent wrote into it, and the cap is
// here for the same reason the document store has one: a copy that runs until EOF
// from a source this process does not control is a copy that fills the disk. 2 GiB
// is past any workspace worth saving and well inside a control plane's free space;
// a machine over it is a machine to look at, not to silently truncate.
const MaxWorkspaceBytes = 2 << 30

var (
	// ErrConfig means the syncer cannot be built from its configuration.
	ErrConfig = errors.New("syncer: invalid configuration")

	// ErrWorkspaceTooLarge means the workspace exceeded MaxWorkspaceBytes. Nothing
	// is stored: a truncated workspace is worse than a loud failure, because the
	// next machine would restore from it.
	ErrWorkspaceTooLarge = errors.New("syncer: the workspace is larger than the cap")

	// ErrBadMachineID means the id cannot be part of an object key. The id comes
	// from the provider; a key is not a path, but a store that treats it as one
	// would put one machine's workspace where another's belongs.
	ErrBadMachineID = errors.New("syncer: the machine id is not usable in an object key")
)

// Fetcher streams a machine's workspace tarball.
//
// It returns the bytes the machine produced, or an error. It must not return an
// empty stream to mean "the machine is gone" -- that means the machine tarred an
// empty workspace -- because the two have opposite answers here.
//
// **The returned reader must unblock when ctx is done.** Sync can notice a
// cancellation between reads, and does, but it cannot interrupt a Read that never
// returns; a transport that honours ctx is the only thing that can. This is part
// of the contract rather than an assumption because the failure it prevents is a
// shutdown that waits for a machine that has already stopped answering.
type Fetcher interface {
	Fetch(ctx context.Context, id firefly.MachineID) (io.ReadCloser, error)
}

// Sink stores one object.
//
// The body is a seekable file rather than a stream so that the sink can size the
// object before sending it and rewind it for a retry. size is that size in bytes.
type Sink interface {
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error
}

// Config is where one deployment's workspaces live, and how they are reached.
type Config struct {
	// Fetcher gets the bytes from the machine. Required.
	Fetcher Fetcher

	// Sink stores them. Required.
	Sink Sink

	// Prefix is the object-key prefix, one path element or more ("state",
	// "verge/prod"). Required: without it, two deployments share an object space
	// and the first id collision overwrites the other's workspace.
	Prefix string

	// MaxBytes is how large one workspace may be. Zero means MaxWorkspaceBytes.
	//
	// It is configurable because the right ceiling is a property of the deployment
	// -- a control plane with a small disk wants a small one -- and because a test
	// that has to write two gigabytes to check the refusal is a test that runs
	// alone on a machine nobody else is using.
	MaxBytes int64
}

// Syncer saves workspaces. It satisfies firefly.Syncer.
type Syncer struct {
	fetch  Fetcher
	sink   Sink
	prefix string
	max    int64
}

var _ firefly.Syncer = (*Syncer)(nil)

// New validates the configuration and returns the syncer.
//
// Nothing is dialled or read here: a syncer that failed at construction could not
// be built inside an offline test tier, and firefly asks for a save long after the
// process started.
func New(cfg Config) (*Syncer, error) {
	if cfg.Fetcher == nil {
		return nil, fmt.Errorf("%w: no Fetcher", ErrConfig)
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("%w: no Sink", ErrConfig)
	}

	prefix := strings.TrimSuffix(cfg.Prefix, "/")
	if prefix == "" {
		return nil, fmt.Errorf("%w: no Prefix", ErrConfig)
	}
	for _, elem := range strings.Split(prefix, "/") {
		if !validElem(elem) {
			return nil, fmt.Errorf("%w: Prefix %q is not an object-key prefix", ErrConfig, cfg.Prefix)
		}
	}

	max := cfg.MaxBytes
	switch {
	case max < 0:
		return nil, fmt.Errorf("%w: MaxBytes cannot be negative", ErrConfig)
	case max == 0:
		max = MaxWorkspaceBytes
	case max < 1024:
		// A cap below a kilobyte is not a workspace, and a caller who set it by
		// accident would rather hear about it than watch every save be refused.
		return nil, fmt.Errorf("%w: MaxBytes %d is smaller than a tarball header", ErrConfig, max)
	}

	return &Syncer{fetch: cfg.Fetcher, sink: cfg.Sink, prefix: prefix, max: max}, nil
}

// Sync saves the machine's workspace, or reports why it could not.
//
// The order is deliberate: fetch, spool, store. A store is never attempted for a
// workspace that could not be fetched or does not fit, so a failed save cannot
// leave an empty object where the last good copy was.
func (s *Syncer) Sync(ctx context.Context, id firefly.MachineID) error {
	if !validElem(string(id)) {
		return fmt.Errorf("%w: %q", ErrBadMachineID, id)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	rc, err := s.fetch.Fetch(ctx, id)
	if err != nil {
		return fmt.Errorf("syncer: fetching %s: %w", id, err)
	}
	defer rc.Close()

	f, err := os.CreateTemp("", "verge-workspace-*")
	if err != nil {
		return fmt.Errorf("syncer: spooling %s: %w", id, err)
	}
	// Removed on every path, including the failures below: a spool file per save,
	// left behind, is how a control plane fills the disk it was protecting.
	defer os.Remove(f.Name())

	// One byte past the cap is enough to refuse it, and reading more would be the
	// copy this limit exists to prevent. The reader is wrapped so a shutdown is
	// noticed between reads, which is the one thing this half can do about a stream
	// that keeps trickling.
	size, err := io.Copy(f, io.LimitReader(&cancelOnRead{ctx: ctx, r: rc}, s.max+1))
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("syncer: spooling %s: %w", id, err)
	}

	if size == 0 {
		// Nothing on the machine: firefly's contract calls this a successful save.
		return nil
	}
	if size > s.max {
		return fmt.Errorf("%w: %s is over %d bytes", ErrWorkspaceTooLarge, id, s.max)
	}

	body, err := os.Open(f.Name())
	if err != nil {
		return fmt.Errorf("syncer: reopening the spool for %s: %w", id, err)
	}
	defer body.Close()

	key := s.prefix + "/" + string(id) + ".tar"
	if err := s.sink.Put(ctx, key, body, size); err != nil {
		return fmt.Errorf("syncer: storing %s: %w", key, err)
	}
	return nil
}

// cancelOnRead makes a copy notice a cancelled context between reads.
//
// It cannot unblock a Read that never returns -- nothing in this process can, and
// that is why Fetcher's contract puts that obligation on the reader -- but a
// stream that trickles for longer than the shutdown window is the ordinary
// failure, and this is where that one is caught.
type cancelOnRead struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancelOnRead) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// validElem reports whether s can be one element of an object key.
//
// A key is not a path, but the two are close enough that a provider-chosen value
// containing a separator or a dot-dot segment is a value that lands somewhere
// nobody chose. A leading dot is refused too: it is how a hidden file is spelled,
// and no machine this repository mints is named that way.
func validElem(s string) bool {
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
