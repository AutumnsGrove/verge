package firefly

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func validDoc() Document {
	return Document{
		Version:  DocumentVersion,
		Machines: []Machine{readyMachine(machineID("m1"))},
		Leases:   []Lease{leaseOn("l1", machineID("m1"), docBase, time.Hour)},
	}
}

// TestMemStoreIsAStore keeps the implementations honest about the interface.
// The compile-time assertion lives here rather than in store.go so the
// production file does not import testing's neighbours for a type check.
var _ Store = (*MemStore)(nil)

func TestMemStoreFirstLoad(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	t.Run("sad/unsaved_store_reports_no_document", func(t *testing.T) {
		if _, err := s.Load(ctx); !errors.Is(err, ErrNoDocument) {
			t.Fatalf("Load() = %v, want ErrNoDocument", err)
		}
	})

	t.Run("happy/after_save_it_loads", func(t *testing.T) {
		if err := s.Save(ctx, validDoc()); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(got.Machines) != 1 || got.Machines[0].ID != machineID("m1") {
			t.Fatalf("Load() = %+v, want the saved machine", got)
		}
		if got.Version != DocumentVersion {
			t.Fatalf("Version = %d, want %d", got.Version, DocumentVersion)
		}
	})
}

// TestMemStoreRefusesInvalidDocument is the failure mode that matters: a bad
// write must not destroy good state.
func TestMemStoreRefusesInvalidDocument(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	if err := s.Save(ctx, validDoc()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	broken := validDoc()
	broken.Machines = append(broken.Machines, broken.Machines[0]) // duplicate id

	if err := s.Save(ctx, broken); err == nil {
		t.Fatal("Save accepted a document with a duplicate machine")
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load after a rejected save: %v", err)
	}
	if len(got.Machines) != 1 {
		t.Fatalf("a rejected save replaced the good document: %d machines", len(got.Machines))
	}
	if s.Saves() != 1 {
		t.Fatalf("Saves() = %d after one rejected write, want 1", s.Saves())
	}
}

// TestMemStoreCopiesBothWays is the aliasing bug the Clone method exists for: a
// store handing out its own slice makes one caller's append another's corrupt
// state, and it surfaces only under an unlucky interleaving.
func TestMemStoreCopiesBothWays(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	doc := validDoc()
	if err := s.Save(ctx, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Run("bad/mutating_after_save_is_not_stored", func(t *testing.T) {
		doc.Machines[0].State = Draining
		got, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Machines[0].State != Ready {
			t.Fatalf("stored state followed the caller's slice: %s", got.Machines[0].State)
		}
	})

	t.Run("bad/mutating_a_loaded_document_is_not_stored", func(t *testing.T) {
		first, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		first.Machines[0].State = Syncing

		second, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if second.Machines[0].State != Ready {
			t.Fatalf("a loaded document aliased the store: %s", second.Machines[0].State)
		}
	})
}

func TestMemStoreContext(t *testing.T) {
	t.Run("bad/cancelled_load", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := NewMemStore().Load(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load with a cancelled context = %v, want context.Canceled", err)
		}
	})

	t.Run("bad/cancelled_save", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := NewMemStore()
		if err := s.Save(ctx, validDoc()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Save with a cancelled context = %v, want context.Canceled", err)
		}
		if s.Saves() != 0 {
			t.Fatalf("a cancelled save counted as %d saves", s.Saves())
		}
	})
}

// TestMemStoreIsConcurrencySafe is the only test here that needs the race
// detector to mean anything. Tick and the HTTP surface both touch the store, so
// "probably fine" is not a good enough answer for a shared document.
func TestMemStoreIsConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	if err := s.Save(ctx, validDoc()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := s.Load(ctx); err != nil {
					t.Errorf("Load: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := s.Save(ctx, validDoc()); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
