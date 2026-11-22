package firefly

import (
	"context"
	"fmt"
	"sync"
)

// Store is where the document lives between ticks and between restarts.
//
// The interface is deliberately whole-document rather than per-machine. A
// partial update would let a tick fail halfway and leave a machine recorded as
// terminating with its lease still holding it open, which is precisely the state
// the ordering property exists to make impossible. Save writes everything or
// nothing.
//
// Implementations must make Save atomic with respect to a concurrent Load: a
// reader sees the previous document or the new one, never a blend. Both
// implementations in this package are safe to call from several goroutines.
type Store interface {
	// Load returns the last saved document. It returns ErrNoDocument when
	// nothing has been saved yet, which is not a failure: a fresh deployment
	// has no state and should reconcile against the cloud instead.
	Load(ctx context.Context) (Document, error)

	// Save replaces the document. Implementations must reject a document that
	// fails Validate, because persisting a broken one loses the good one.
	Save(ctx context.Context, doc Document) error
}

// MemStore is an in-memory Store.
//
// It is what the unit and integration tiers run against: fast, deterministic,
// and shareable with a fake clock. It is not a cache and holds no eviction
// policy, because its only job is to be a store a test can trust.
type MemStore struct {
	mu     sync.Mutex
	doc    Document
	loaded bool // false until the first successful Save
	saves  int  // observed by tests that assert save counts
}

// NewMemStore returns an empty store, which reports ErrNoDocument until saved.
func NewMemStore() *MemStore { return &MemStore{} }

// Load returns a copy of the stored document.
func (s *MemStore) Load(ctx context.Context) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return Document{}, ErrNoDocument
	}
	return s.doc.Clone(), nil
}

// Save validates and stores a copy of doc.
//
// The document is validated before it is copied, so a rejected Save leaves the
// previous document intact — the failure mode that matters, because the
// alternative is losing good state to a bad write.
func (s *MemStore) Save(ctx context.Context, doc Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := doc.Validate(); err != nil {
		return fmt.Errorf("firefly: refusing to save an invalid document: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = doc.Clone()
	s.loaded = true
	s.saves++
	return nil
}

// Saves reports how many successful saves have happened. It exists so a test can
// assert that Tick writes once per change and not once per machine, which is a
// property the design claims and nothing else would notice.
func (s *MemStore) Saves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}
