package fake

import (
	"context"
	"errors"
	"sync"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// ErrSyncFailed is what the fake syncer returns when a test asks it to fail.
var ErrSyncFailed = errors.New("fake: the machine's state could not be saved")

// Syncer records what was asked of it and fails on demand.
//
// It keeps its own lock rather than sharing the cloud's, because a sync is not a
// provider call: the machine's storage is a different system with a different
// failure mode, and that is the whole reason it is a separate dependency.
type Syncer struct {
	mu sync.Mutex

	remaining int  // fail this many more syncs
	always    bool // fail every sync

	calls  int
	synced []firefly.MachineID
}

// NewSyncer returns a syncer that always succeeds.
func NewSyncer() *Syncer { return &Syncer{} }

var _ firefly.Syncer = (*Syncer)(nil)

// FailNext makes the next n syncs fail.
func (s *Syncer) FailNext(n int) *Syncer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining = n
	return s
}

// FailAlways makes every sync fail, which is what the cap has to abandon.
func (s *Syncer) FailAlways() *Syncer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.always = true
	return s
}

// Sync saves, or refuses.
func (s *Syncer) Sync(ctx context.Context, id firefly.MachineID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++

	if s.always {
		return ErrSyncFailed
	}
	if s.remaining > 0 {
		s.remaining--
		return ErrSyncFailed
	}
	s.synced = append(s.synced, id)
	return nil
}

// Calls reports how many syncs were attempted, including the failed ones.
func (s *Syncer) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Synced reports the machines that were saved successfully, in order.
func (s *Syncer) Synced() []firefly.MachineID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]firefly.MachineID, len(s.synced))
	copy(out, s.synced)
	return out
}
