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

// TestNodeAnswersDoNotDependOnOrder states the property the caching bug violated,
// by running each scenario twice: once asking at every step, once asking only at
// the end.
//
// The first version of this test asserted a fixed answer and used two separate
// fakes, so it passed with a cache in place -- it compared nothing. The property
// is not "the answer is true", it is "asking earlier cannot change the answer
// later", and the only way to test that is to run the same history both ways.
//
// The discriminating case is an observation during the join delay followed by a
// delete before the join instant lands: the honest answer is false forever, while
// a cache primed on the first ask reports true for good.
type step struct {
	advance time.Duration
	delete  bool
	forget  bool
}

func TestNodeAnswersDoNotDependOnOrder(t *testing.T) {
	ctx := context.Background()

	scenarios := []struct {
		name      string
		boot      time.Duration
		joinDelay time.Duration
		// reap404 puts the delete on the 404 path, which reaps without a
		// deleteAt. It is a separate scenario flag rather than a step, because
		// the knob lives on the cloud and applies to every later delete.
		reap404 bool
		steps   []step
	}{
		{
			name: "sad/primed_in_the_join_delay_then_deleted_before_joining",
			boot: 0, joinDelay: time.Minute,
			steps: []step{{advance: 30 * time.Second}, {delete: true}, {advance: time.Hour}},
		},
		{
			name: "sad/primed_while_booting_then_deleted",
			boot: 2 * time.Minute, joinDelay: 0,
			steps: []step{{advance: time.Minute}, {delete: true}, {advance: time.Hour}},
		},
		{
			// The 404 path reaps without a deleteAt, so a cache keyed on deleteAt
			// is blind to it. This scenario is the one that catches that shape.
			name: "sad/primed_before_boot_then_404_reaped",
			boot: 2 * time.Minute, joinDelay: 0, reap404: true,
			steps: []step{{advance: time.Second}, {delete: true}, {advance: time.Hour}},
		},
		{
			name: "happy/joined_then_404_reaped",
			boot: 0, joinDelay: 0, reap404: true,
			steps: []step{{advance: time.Minute}, {delete: true}, {advance: time.Hour}},
		},
		{
			name: "happy/primed_in_the_join_delay_then_joins",
			boot: 0, joinDelay: time.Minute,
			steps: []step{{advance: 30 * time.Second}, {advance: time.Minute}},
		},
		{
			name: "happy/joined_then_deleted",
			boot: 0, joinDelay: 0,
			steps: []step{{advance: time.Minute}, {delete: true}, {advance: time.Hour}},
		},
		{
			name: "happy/joined_then_deleted_then_forgotten",
			boot: 0, joinDelay: 0,
			steps: []step{{advance: time.Minute}, {delete: true}, {advance: time.Hour}, {forget: true}},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// run replays the scenario, asking after every step when observe is
			// true, and returns the final answer either way.
			run := func(observe bool) (exists, joined bool) {
				c := New(fakeBase)
				c.Boot(sc.boot)
				if sc.reap404 {
					c.DeleteReturns404()
				}
				n := NewNetwork(c).JoinDelay(sc.joinDelay)

				id, err := c.Create(ctx, spec())
				if err != nil {
					t.Fatalf("Create: %v", err)
				}

				ask := func() {
					if !observe {
						return
					}
					if _, err := n.NodeExists(ctx, id); err != nil {
						t.Fatalf("NodeExists: %v", err)
					}
					if _, err := n.Joined(ctx, id); err != nil {
						t.Fatalf("Joined: %v", err)
					}
				}

				ask()
				for _, st := range sc.steps {
					if st.advance > 0 {
						c.Advance(st.advance)
					}
					if st.delete {
						if err := c.Delete(ctx, id); err != nil {
							t.Fatalf("Delete: %v", err)
						}
					}
					if st.forget {
						if err := n.Forget(ctx, id); err != nil {
							t.Fatalf("Forget: %v", err)
						}
					}
					ask()
				}

				exists, err = n.NodeExists(ctx, id)
				if err != nil {
					t.Fatalf("NodeExists: %v", err)
				}
				joined, err = n.Joined(ctx, id)
				if err != nil {
					t.Fatalf("Joined: %v", err)
				}
				return exists, joined
			}

			quietExists, quietJoined := run(false)
			askedExists, askedJoined := run(true)

			if quietExists != askedExists {
				t.Errorf("NodeExists depends on when it was asked: %v unobserved, %v observed",
					quietExists, askedExists)
			}
			if quietJoined != askedJoined {
				t.Errorf("Joined depends on when it was asked: %v unobserved, %v observed",
					quietJoined, askedJoined)
			}
		})
	}
}
