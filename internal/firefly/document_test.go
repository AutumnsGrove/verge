package firefly

import (
	"errors"
	"testing"
	"time"
)

// docBase is fixed so that every case reads as an offset from one instant.
// Document tests never call time.Now, for the same reason the rest do not.
var docBase = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func machineID(s string) MachineID { return MachineID(s) }

// readyMachine is a valid, boring machine: created, capped, and sitting in Ready.
func readyMachine(id MachineID) Machine {
	return Machine{
		ID:        id,
		State:     Ready,
		CreatedAt: docBase,
		Deadline:  docBase.Add(4 * time.Hour),
		Since:     docBase.Add(time.Minute),
	}
}

func leaseOn(id LeaseID, m MachineID, start time.Time, ttl time.Duration) Lease {
	return Lease{
		ID:      id,
		Machine: m,
		Holder:  "session:01J8",
		Kind:    Session,
		Start:   start,
		TTL:     ttl,
	}
}

func TestDocumentValidate(t *testing.T) {
	syncError := func(m Machine) Machine {
		m.State = Error
		m.Origin = Syncing
		m.Attempt = 2
		m.LastErr = "sync failed"
		return m
	}

	cases := []struct {
		name    string
		doc     Document
		isErr   bool  // the document must be rejected
		wantErr error // when set, the rejection must wrap this
	}{
		{
			name: "happy/empty_document",
			doc:  Document{Version: DocumentVersion},
		},
		{
			name: "happy/one_ready_machine",
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
			},
		},
		{
			name: "happy/machine_with_live_lease",
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
				Leases:   []Lease{leaseOn("l1", machineID("m1"), docBase, time.Hour)},
			},
		},
		{
			name: "happy/error_with_origin",
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{syncError(readyMachine(machineID("m1")))},
			},
		},
		{
			name:    "sad/unsupported_version",
			doc:     Document{Version: DocumentVersion + 1},
			isErr:   true,
			wantErr: ErrDocumentVersion,
		},
		{
			name:  "sad/error_origin_without_a_retry_row",
			isErr: true,
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{func() Machine {
					m := readyMachine(machineID("m1"))
					m.State, m.Origin, m.Attempt, m.LastErr = Error, Absent, 1, "boom"
					return m
				}()},
			},
		},
		{
			name:  "sad/lease_on_a_machine_that_is_gone",
			isErr: true,
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
				Leases:   []Lease{leaseOn("l1", machineID("m9"), docBase, time.Hour)},
			},
		},
		{
			name:  "sad/deadline_not_after_creation",
			isErr: true,
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{func() Machine {
					m := readyMachine(machineID("m1"))
					m.Deadline = m.CreatedAt
					return m
				}()},
			},
		},
		{
			name:  "bad/duplicate_machine",
			isErr: true,
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{
					readyMachine(machineID("m1")),
					readyMachine(machineID("m1")),
				},
			},
		},
		{
			name:  "bad/empty_machine_id",
			isErr: true,
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID(""))},
			},
		},
		{
			name:  "bad/duplicate_lease",
			isErr: true,
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
				Leases: []Lease{
					leaseOn("l1", machineID("m1"), docBase, time.Hour),
					leaseOn("l1", machineID("m1"), docBase, time.Hour),
				},
			},
		},
		{
			name:  "bad/unknown_lease_kind",
			isErr: true,
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
				Leases: []Lease{func() Lease {
					l := leaseOn("l1", machineID("m1"), docBase, time.Hour)
					l.Kind = Kind("haunting")
					return l
				}()},
			},
		},
		{
			name:  "bad/zero_lease_ttl",
			isErr: true,
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
				Leases:   []Lease{leaseOn("l1", machineID("m1"), docBase, 0)},
			},
		},
		{
			name:  "bad/lease_without_a_holder",
			isErr: true,
			doc: Document{
				Version:  DocumentVersion,
				Machines: []Machine{readyMachine(machineID("m1"))},
				Leases: []Lease{func() Lease {
					l := leaseOn("l1", machineID("m1"), docBase, time.Hour)
					l.Holder = ""
					return l
				}()},
			},
		},
		{
			// A machine that is not in Error carrying an attempt count is how a
			// later tick retries a step the machine is no longer doing.
			name:  "bad/stale_attempt_outside_error",
			isErr: true,
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{func() Machine {
					m := readyMachine(machineID("m1"))
					m.Attempt = 3
					return m
				}()},
			},
		},
		{
			name:  "bad/stale_origin_outside_error",
			isErr: true,
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{func() Machine {
					m := readyMachine(machineID("m1"))
					m.Origin = Syncing
					return m
				}()},
			},
		},
		{
			name:  "bad/since_before_creation",
			isErr: true,
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{func() Machine {
					m := readyMachine(machineID("m1"))
					m.Since = m.CreatedAt.Add(-time.Minute)
					return m
				}()},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.doc.Validate()
			if !c.isErr {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want the document rejected")
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("Validate() = %v, want it to wrap %v", err, c.wantErr)
			}
		})
	}
}

// TestErrorOriginMustHaveARetryRow pins the coupling between validation and the
// transition table. If a row is added or removed, this is where the document
// format notices.
func TestErrorOriginMustHaveARetryRow(t *testing.T) {
	valid := retryOrigins()
	if len(valid) == 0 {
		t.Fatal("retryOrigins() is empty, so ERROR could never be retried")
	}
	for origin := range valid {
		doc := Document{
			Version: DocumentVersion,
			Machines: []Machine{{
				ID: machineID("m1"), State: Error, Origin: origin,
				Attempt: 1, LastErr: "boom", CreatedAt: docBase,
				Deadline: docBase.Add(time.Hour), Since: docBase,
			}},
		}
		if err := doc.Validate(); err != nil {
			t.Errorf("origin %s has a retry row but does not validate: %v", origin, err)
		}
	}
}

func TestDocumentClone(t *testing.T) {
	doc := Document{
		Version:  DocumentVersion,
		Machines: []Machine{readyMachine(machineID("m1"))},
		Leases:   []Lease{leaseOn("l1", machineID("m1"), docBase, time.Hour)},
	}
	clone := doc.Clone()

	clone.Machines[0].State = Draining
	clone.Leases[0].TTL = time.Minute

	if doc.Machines[0].State != Ready {
		t.Fatalf("mutating the clone changed the original machine: %s", doc.Machines[0].State)
	}
	if doc.Leases[0].TTL != time.Hour {
		t.Fatalf("mutating the clone changed the original lease ttl: %s", doc.Leases[0].TTL)
	}
}

func TestDocumentAccessors(t *testing.T) {
	doc := Document{
		Version: DocumentVersion,
		Machines: []Machine{
			readyMachine(machineID("m1")),
			readyMachine(machineID("m2")),
		},
		Leases: []Lease{
			leaseOn("l1", machineID("m1"), docBase, time.Hour),
			leaseOn("l2", machineID("m2"), docBase, time.Hour),
			// A lapsed lease on m1: LeasesFor must still report it, because
			// Pruned is the function that decides what to forget.
			leaseOn("l3", machineID("m1"), docBase, -time.Hour),
		},
	}

	t.Run("happy/machine_found", func(t *testing.T) {
		m, ok := doc.Machine(machineID("m2"))
		if !ok || m.ID != machineID("m2") {
			t.Fatalf("Machine(m2) = %v, %v", m, ok)
		}
	})

	t.Run("sad/machine_missing", func(t *testing.T) {
		if _, ok := doc.Machine(machineID("m9")); ok {
			t.Fatal("Machine(m9) reported a machine that is not there")
		}
	})

	t.Run("happy/leases_for_includes_lapsed", func(t *testing.T) {
		if got := len(doc.LeasesFor(machineID("m1"))); got != 2 {
			t.Fatalf("LeasesFor(m1) = %d leases, want 2 (including the lapsed one)", got)
		}
	})

	t.Run("sad/leases_for_unknown_machine", func(t *testing.T) {
		if got := doc.LeasesFor(machineID("m9")); got != nil {
			t.Fatalf("LeasesFor(m9) = %v, want nil", got)
		}
	})
}

func TestDocumentPruned(t *testing.T) {
	doc := Document{
		Version:  DocumentVersion,
		Machines: []Machine{readyMachine(machineID("m1"))},
		Leases: []Lease{
			leaseOn("live", machineID("m1"), docBase, time.Hour),
			leaseOn("lapsed", machineID("m1"), docBase, time.Minute),
			leaseOn("equally_lapsed", machineID("m1"), docBase.Add(-time.Hour), time.Hour),
		},
	}

	t.Run("happy/keeps_live_drops_lapsed", func(t *testing.T) {
		out, removed := doc.Pruned(docBase.Add(30 * time.Minute))
		if removed != 2 {
			t.Fatalf("removed %d leases, want 2", removed)
		}
		if len(out.Leases) != 1 || out.Leases[0].ID != "live" {
			t.Fatalf("Pruned kept %v, want only the live lease", out.Leases)
		}
		if err := out.Validate(); err != nil {
			t.Fatalf("a pruned document does not validate: %v", err)
		}
	})

	t.Run("happy/pruning_leaves_the_original_alone", func(t *testing.T) {
		if _, removed := doc.Pruned(docBase.Add(30 * time.Minute)); removed != 2 {
			t.Fatalf("removed %d, want 2", removed)
		}
		if len(doc.Leases) != 3 {
			t.Fatalf("Pruned mutated the receiver: %d leases remain, want 3", len(doc.Leases))
		}
	})

	// At docBase the window is [Start, Start+TTL), so both leases that start
	// there are live and only the one that expired exactly at docBase is not.
	t.Run("sad/only_the_one_that_expired_at_this_instant", func(t *testing.T) {
		out, removed := doc.Pruned(docBase)
		if removed != 1 {
			t.Fatalf("removed %d at the start instant, want 1", removed)
		}
		if len(out.Leases) != 2 {
			t.Fatalf("kept %d leases, want 2", len(out.Leases))
		}
	})

	t.Run("bad/empty_document", func(t *testing.T) {
		out, removed := (Document{Version: DocumentVersion}).Pruned(docBase)
		if removed != 0 || len(out.Leases) != 0 {
			t.Fatalf("Pruned on an empty document = %d removed, %d kept", removed, len(out.Leases))
		}
	})
}
