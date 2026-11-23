package cloudinit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var goodSpec = Spec{
	Hostname: "verge-machine-1",
	MeshAddr: "https://control.example.ts.net",
	AuthKey:  "tskey-auth-k1234CNTRL-abcdef",
}

// parsed is one rendering, decoded back into the shapes a machine would see.
type parsed struct {
	hostname string
	facts    joinFile
	runcmd   []string
}

// parse decodes a rendering the way cloud-init and the agent will, and fails the test
// if it cannot — which is the property the fuzz target asserts for arbitrary input.
func parse(t *testing.T, out []byte) parsed {
	t.Helper()

	text := string(out)
	if !strings.HasPrefix(text, Header) {
		t.Fatalf("the document does not start with %q: %q", Header, firstLine(text))
	}
	// cloud-init rejects anything before the marker, so nothing may be.
	if strings.Index(text, Header) != 0 {
		t.Fatalf("the marker is not on the first line: %q", firstLine(text))
	}

	var doc struct {
		Hostname         string      `yaml:"hostname"`
		PreserveHostname bool        `yaml:"preserve_hostname"`
		WriteFiles       []writeFile `yaml:"write_files"`
		RunCmd           []string    `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(text, Header)), &doc); err != nil {
		t.Fatalf("the rendering is not valid YAML: %v\n%s", err, text)
	}
	// cloud-init returns early from cc_set_hostname and cc_update_hostname when this
	// flag is true, so a document that sets it does not name the machine. The field is
	// decoded here only so this can see it — and this has to come after the unmarshal,
	// or it is an assertion about the zero value. It did, once.
	if doc.PreserveHostname {
		t.Fatal("preserve_hostname is set, which makes cloud-init skip the hostname: key entirely")
	}
	if len(doc.WriteFiles) != 1 {
		t.Fatalf("%d write_files entries, want 1", len(doc.WriteFiles))
	}
	if doc.WriteFiles[0].Path != Path {
		t.Fatalf("write_files path = %q, want %q", doc.WriteFiles[0].Path, Path)
	}
	if got := doc.WriteFiles[0].Permissions; got != "0600" {
		t.Fatalf("permissions = %q, want 0600: the key must not be readable by others", got)
	}

	var facts joinFile
	if err := json.Unmarshal([]byte(doc.WriteFiles[0].Content), &facts); err != nil {
		t.Fatalf("the join file is not JSON: %v\n%q", err, doc.WriteFiles[0].Content)
	}

	return parsed{hostname: doc.Hostname, facts: facts, runcmd: doc.RunCmd}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func TestRender(t *testing.T) {
	t.Run("happy/a_machine_is_told_three_facts", func(t *testing.T) {
		out, err := Render(goodSpec)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		got := parse(t, out)

		// The hostname is the one the network will later be asked about, so it has to
		// survive byte for byte.
		if got.hostname != goodSpec.Hostname {
			t.Fatalf("hostname = %q, want %q", got.hostname, goodSpec.Hostname)
		}
		if got.facts.Hostname != goodSpec.Hostname {
			t.Fatalf("the join file names the machine %q, want %q", got.facts.Hostname, goodSpec.Hostname)
		}
		if got.facts.MeshAddr != goodSpec.MeshAddr || got.facts.AuthKey != goodSpec.AuthKey {
			t.Fatalf("join facts = %+v", got.facts)
		}
		if len(got.runcmd) != 1 || !strings.Contains(got.runcmd[0], Path) {
			t.Fatalf("runcmd = %v, want the image's join command", got.runcmd)
		}
	})

	// The renderer is the only thing that decides what runs as root at boot, so the
	// document is asserted whole rather than by key: a field appearing here without a
	// review is a field a machine executes.
	t.Run("happy/the_document_is_this_and_nothing_else", func(t *testing.T) {
		out, err := Render(goodSpec)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}

		var keys map[string]any
		if err := yaml.Unmarshal([]byte(strings.TrimPrefix(string(out), Header)), &keys); err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := map[string]bool{"hostname": true, "write_files": true, "runcmd": true}
		for k := range keys {
			if !want[k] {
				t.Errorf("the boot script carries an unreviewed key %q", k)
			}
			delete(want, k)
		}
		for k := range want {
			t.Errorf("the boot script is missing %q", k)
		}
	})

	t.Run("happy/no_credential_that_can_spend_money", func(t *testing.T) {
		// The one secret that may reach a machine is a single-use mesh key. A field
		// named after a provider would be the end of that guarantee, so the absence is
		// asserted rather than assumed: Spec has no such field and the output cannot
		// invent one.
		out, err := Render(goodSpec)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		for _, banned := range []string{"token", "apiKey", "api_key", "secret", "password"} {
			if strings.Contains(strings.ToLower(string(out)), strings.ToLower(banned)) {
				t.Errorf("the boot script mentions %q", banned)
			}
		}
	})

	cases := []struct {
		name string
		spec Spec
	}{
		{"sad/no_hostname", Spec{MeshAddr: goodSpec.MeshAddr, AuthKey: goodSpec.AuthKey}},
		{"sad/no_mesh_address", Spec{Hostname: goodSpec.Hostname, AuthKey: goodSpec.AuthKey}},
		{"sad/no_key", Spec{Hostname: goodSpec.Hostname, MeshAddr: goodSpec.MeshAddr}},
		{"bad/nothing_at_all", Spec{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(tc.spec)
			if !errors.Is(err, ErrSpec) {
				t.Fatalf("Render = %v, want ErrSpec", err)
			}
			if out != nil {
				t.Fatalf("Render returned %d bytes with an error", len(out))
			}
		})
	}
}

// TestHostileInputCannotEscape is the case the fuzzer generalises. Each input is
// something a caller could pass by accident or on purpose, and each one has to come
// back out as the same string rather than as new YAML.
func TestHostileInputCannotEscape(t *testing.T) {
	hostile := []string{
		"a\nhostname: attacker",
		"a: b",
		"- item",
		"\"quoted\"",
		"'single'",
		"$(reboot)",
		"`reboot`",
		"a\tb",
		"a#comment",
		"&anchor",
		"*alias",
		"|block",
		">folded",
		"{flow}",
		"[seq]",
		"%TAG",
		"@reserved",
		"!!binary",
		"-",
		"---",
		"...",
		"\r\n",
		// The three the fuzzer found: the encoder writes these as block scalars,
		// which for a value that begins or ends with a newline is not reversible.
		"\n",
		"\na",
		"\n\n",
		"\u2028",
		"\u2029",
		"\\n",
		"",
	}

	for _, value := range hostile {
		t.Run(strings.ReplaceAll(firstLine(value), "\r", ""), func(t *testing.T) {
			// A value may only be refused for being empty or not being text at all;
			// otherwise it has to round-trip exactly, whatever it contains. The
			// renderer is not a hostname validator — the provider does that, and
			// refuses the create before anything boots.
			out, err := Render(Spec{Hostname: value, MeshAddr: value, AuthKey: value})
			if err != nil {
				if value == "" || !utf8.ValidString(value) {
					if !errors.Is(err, ErrSpec) {
						t.Fatalf("Render(%q) = %v, want ErrSpec", value, err)
					}
					return
				}
				t.Fatalf("Render(%q) = %v", value, err)
			}
			got := parse(t, out)
			if got.hostname != value {
				t.Fatalf("hostname escaped its value: got %q, want %q", got.hostname, value)
			}
			if got.facts.AuthKey != value || got.facts.MeshAddr != value {
				t.Fatalf("join facts escaped their values: %+v", got.facts)
			}
		})
	}
}
