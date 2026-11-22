package fake

import (
	"context"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// TestNodeTruthTable asserts the one fact the deletion rule leans on, directly and
// exhaustively, instead of leaving it to be inferred from lifecycle tests.
//
// It is here because four separate findings on the network half were all the same
// bug: the fake reporting that a node existed when it never had, or the reverse.
// Each was found through a different lifecycle path, and each was fixed where it
// surfaced. A table over the inputs that decide the answer finds them all at once,
// and it fails when someone adds a fifth path.
//
// The two questions are deliberately separate, because collapsing them is the
// mistake the two-method split exists to prevent:
//
//	NodeExists -- did a node ever appear, and is the record still there?
//	Joined     -- is that node usable right now, i.e. is the machine running?
type nodeCase struct {
	name string

	// setup is applied before the machine is created.
	boot            time.Duration
	stall           bool
	joinDelay       time.Duration
	metadataMissing bool

	// build drives the machine from creation to the state under test.
	build func(t *testing.T, c *Cloud, n *Network) firefly.MachineID

	wantExists bool
	wantJoined bool
}

func TestNodeTruthTable(t *testing.T) {
	ctx := context.Background()

	create := func(t *testing.T, c *Cloud) firefly.MachineID {
		t.Helper()
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}

	cases := []nodeCase{
		{
			name: "happy/running_and_joined",
			boot: 0,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(time.Minute)
				return id
			},
			wantExists: true, wantJoined: true,
		},
		{
			name: "happy/joined_after_the_join_delay",
			boot: 0, joinDelay: time.Minute,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(2 * time.Minute)
				return id
			},
			wantExists: true, wantJoined: true,
		},
		{
			name: "sad/still_booting",
			boot: 2 * time.Minute,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(time.Minute)
				return id
			},
		},
		{
			name:  "sad/boot_never_finishes",
			stall: true,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(24 * time.Hour)
				return id
			},
		},
		{
			name: "sad/running_but_the_join_has_not_landed",
			boot: 0, joinDelay: time.Minute,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(30 * time.Second)
				return id
			},
		},
		{
			name: "sad/metadata_service_never_answered",
			boot: 0, metadataMissing: true,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(24 * time.Hour)
				return id
			},
		},
		{
			name: "sad/deleted_before_boot",
			boot: 2 * time.Minute,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				if err := c.Delete(ctx, id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				c.Advance(time.Hour)
				return id
			},
		},
		{
			name: "sad/deleted_during_the_join_delay",
			boot: time.Minute, joinDelay: time.Minute,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(time.Minute) // running, join due in a minute
				if err := c.Delete(ctx, id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				c.Advance(time.Hour)
				return id
			},
		},
		{
			name: "sad/reaped_404_before_boot",
			boot: 2 * time.Minute,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				c.DeleteReturns404()
				id := create(t, c)
				if err := c.Delete(ctx, id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				c.Advance(time.Hour)
				return id
			},
		},
		{
			name: "sad/joined_then_deleted_lingers",
			boot: 0,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(time.Minute)
				if err := c.Delete(ctx, id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				c.Advance(time.Hour)
				return id
			},
			wantExists: true, wantJoined: false,
		},
		{
			name: "sad/joined_then_reaped_404_lingers",
			boot: 0,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				c.DeleteReturns404()
				id := create(t, c)
				c.Advance(time.Minute)
				if err := c.Delete(ctx, id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				c.Advance(time.Hour)
				return id
			},
			wantExists: true, wantJoined: false,
		},
		{
			name: "sad/forgotten_while_running",
			boot: 0,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(time.Minute)
				if err := n.Forget(ctx, id); err != nil {
					t.Fatalf("Forget: %v", err)
				}
				return id
			},
		},
		{
			name: "sad/forgotten_while_lingering",
			boot: 0,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				id := create(t, c)
				c.Advance(time.Minute)
				if err := c.Delete(ctx, id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				c.Advance(time.Hour)
				if err := n.Forget(ctx, id); err != nil {
					t.Fatalf("Forget: %v", err)
				}
				return id
			},
		},
		{
			name: "bad/machine_that_never_existed",
			boot: 0,
			build: func(t *testing.T, c *Cloud, n *Network) firefly.MachineID {
				return "never-created"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(fakeBase)
			c.Boot(tc.boot)
			if tc.stall {
				c.StallBoot()
			}
			n := NewNetwork(c).JoinDelay(tc.joinDelay)
			if tc.metadataMissing {
				n.MetadataMissing()
			}

			id := tc.build(t, c, n)

			gotExists, err := n.NodeExists(ctx, id)
			if err != nil {
				t.Fatalf("NodeExists: %v", err)
			}
			if gotExists != tc.wantExists {
				t.Errorf("NodeExists = %v, want %v", gotExists, tc.wantExists)
			}

			gotJoined, err := n.Joined(ctx, id)
			if err != nil {
				t.Fatalf("Joined: %v", err)
			}
			if gotJoined != tc.wantJoined {
				t.Errorf("Joined = %v, want %v", gotJoined, tc.wantJoined)
			}

			// Joined must never be true when a record is not there: a usable node
			// that does not exist is the contradiction this whole split prevents.
			if gotJoined && !gotExists {
				t.Errorf("Joined = true while NodeExists = false")
			}
		})
	}
}

// TestNodeAnswersDoNotDependOnOrder is the property the caching bug violated.
//
// Every case is asked twice, after a further observation and without one, and the
// answers must match. It is cheap, and it is the assertion that would have caught
// the cached join instant immediately rather than three reviews later.
func TestNodeAnswersDoNotDependOnOrder(t *testing.T) {
	ctx := context.Background()

	build := func(t *testing.T) (*Cloud, *Network, firefly.MachineID) {
		t.Helper()
		c, n := newBoth(t)
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		c.Advance(time.Minute)
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)
		return c, n, id
	}

	// Asked first after the machine is gone: the unprimed path.
	t.Run("happy/unprimed", func(t *testing.T) {
		_, n, id := build(t)
		if exists, err := n.NodeExists(ctx, id); err != nil || !exists {
			t.Fatalf("NodeExists = %v, %v; want true", exists, err)
		}
	})

	// Asked once before the delete lands, then again after.
	t.Run("happy/primed_then_reasked", func(t *testing.T) {
		c, n := newBoth(t)
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		c.Advance(time.Minute)
		if _, err := n.NodeExists(ctx, id); err != nil {
			t.Fatalf("NodeExists: %v", err)
		}
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		for i := 0; i < 3; i++ {
			if exists, err := n.NodeExists(ctx, id); err != nil || !exists {
				t.Fatalf("NodeExists on ask %d = %v, %v; want true", i+1, exists, err)
			}
		}
	})
}
