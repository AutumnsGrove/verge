package firefly

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestNamePrefix covers the derivation directly, because everything downstream --
// a valid hostname, a unique device in a tailnet, a node someone can match to a
// machine -- depends on this one small function being boring.
func TestNamePrefix(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		value     string
		want      string
		wantExact bool // when false, the case only asserts the shape
	}{
		{"happy/a_deployments_label_becomes_its_prefix", "verge", "machine", "verge-machine", true},
		{"happy/one_word_deployments_read_well", "verge", "prod", "verge-prod", true},
		{"happy/uppercase_is_lowered", "Verge", "PROD", "verge-prod", true},
		{"happy/a_dot_and_an_underscore_become_one_hyphen", "verge", "eu.west_1", "verge-eu-west-1", true},
		{"happy/a_run_of_separators_collapses", "verge", "a...__b", "verge-a-b", true},
		{"sad/nothing_usable_falls_back", "___", "...", "verge", true},
		{"bad/a_very_long_label_is_truncated", "verge", strings.Repeat("x", 200), "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namePrefixFor(tc.key, tc.value)
			if tc.wantExact && got != tc.want {
				t.Fatalf("namePrefixFor(%q, %q) = %q, want %q", tc.key, tc.value, got, tc.want)
			}
			// Whatever the input, the prefix has to be usable as the start of a
			// hostname: that is what the provider will be handed.
			if got == "" || len(got) > maxNamePrefix {
				t.Fatalf("prefix %q is %d characters, want 1..%d", got, len(got), maxNamePrefix)
			}
			if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
				t.Fatalf("prefix %q starts or ends with a hyphen", got)
			}
			for _, r := range got {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					t.Fatalf("prefix %q contains %q, which a hostname cannot", got, r)
				}
			}
		})
	}
}

// TestMintName covers the counter, which is what makes a name unique without a
// random source and stable across a restart.
func TestMintName(t *testing.T) {
	f := &Firefly{namePrefix: "verge-machine"}

	t.Run("happy/the_counter_starts_at_one", func(t *testing.T) {
		doc := &Document{Version: DocumentVersion}
		if got, want := f.mintName(doc), "verge-machine-1"; got != want {
			t.Fatalf("mintName = %q, want %q", got, want)
		}
		if doc.NextMachineNumber != 1 {
			t.Fatalf("NextMachineNumber = %d, want 1", doc.NextMachineNumber)
		}
	})

	t.Run("happy/the_sequence_continues", func(t *testing.T) {
		doc := &Document{Version: DocumentVersion}
		first := f.mintName(doc)
		second := f.mintName(doc)
		if first == second {
			t.Fatalf("two mints produced the same name %q", first)
		}
		if second != "verge-machine-2" {
			t.Fatalf("second mint = %q", second)
		}
	})

	// A lost document restarts the counter, and the machines that survived are
	// recovered by adoption carrying the names they were created with. The next
	// mint has to walk past them, or the document would hold two machines with one
	// name and Validate would refuse every write from then on.
	t.Run("sad/an_adopted_machine_holds_its_name", func(t *testing.T) {
		doc := &Document{Version: DocumentVersion}
		doc.NextMachineNumber = 1
		doc.Machines = []Machine{{ID: "hetzner:7", Name: "verge-machine-2"}}

		got := f.mintName(doc)
		if got != "verge-machine-3" {
			t.Fatalf("mintName = %q, want verge-machine-3", got)
		}
		if doc.NextMachineNumber != 3 {
			t.Fatalf("NextMachineNumber = %d, want 3", doc.NextMachineNumber)
		}
	})

	// Names are handed to a provider that validates them as hostnames, so the
	// longest prefix plus a separator plus the widest counter has to stay within the
	// limit a hostname may occupy.
	t.Run("bad/the_longest_name_still_fits_a_hostname", func(t *testing.T) {
		f := &Firefly{namePrefix: strings.Repeat("a", maxNamePrefix)}
		doc := &Document{Version: DocumentVersion, NextMachineNumber: ^uint64(0) - 1}

		name := f.mintName(doc)
		if len(name) > 63 {
			t.Fatalf("%q is %d characters, and a hostname may only be 63", name, len(name))
		}
		if want := strings.Repeat("a", maxNamePrefix) + "-" + strconv.FormatUint(^uint64(0), 10); name != want {
			t.Fatalf("mintName = %q, want %q", name, want)
		}
	})
}

// TestMintNameIsAHint is the property the caller depends on: asking for a name records
// nothing, and two asks before either provisions agree -- because the answer is only a
// function of the document. The first version of this minted on the live document and made
// two asks disagree, which is the drift it now checks for.
func TestMintNameIsAHint(t *testing.T) {
	f := newFirefly(t)
	ctx := context.Background()

	first, err := f.MintName(ctx)
	if err != nil {
		t.Fatalf("MintName: %v", err)
	}
	if first == "" {
		t.Fatal("MintName returned an empty name")
	}
	if !strings.HasPrefix(first, "verge-machine-") {
		t.Fatalf("MintName = %q, want the label's prefix", first)
	}

	f.mu.Lock()
	counter := f.doc.NextMachineNumber
	f.mu.Unlock()

	second, err := f.MintName(ctx)
	if err != nil {
		t.Fatalf("MintName: %v", err)
	}
	if first != second {
		t.Fatalf("two asks before a provision disagree: %q and %q", first, second)
	}

	f.mu.Lock()
	after := f.doc.NextMachineNumber
	f.mu.Unlock()
	if after != counter {
		t.Fatalf("MintName moved the counter from %d to %d: it is a read", counter, after)
	}

}
