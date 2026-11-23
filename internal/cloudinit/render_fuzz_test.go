package cloudinit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// FuzzCloudInit is the security test the design calls for, with a fuzzer attached:
// these bytes become a script that runs as root at boot.
//
// The property is stronger than "no panic". Any rendering must parse, and every input
// must come back out of the parse byte for byte — which is what makes a newline in a
// name an escaped byte rather than a new key. A rendering that errors is only legal for
// an empty field, because the renderer refuses missing facts rather than emitting a
// document with a hole in it.
func FuzzCloudInit(f *testing.F) {
	f.Add(goodSpec.Hostname, goodSpec.MeshAddr, goodSpec.AuthKey)
	f.Add("host", "https://control.example.ts.net", "tskey-auth-k1-abc")
	f.Add("a\nhostname: evil", "mesh", "key")
	f.Add("", "mesh", "key")
	f.Add("host", "", "key")
	f.Add("host", "mesh", "")
	f.Add("\x00\x01\x02", "\xff\xfe", "\"'\n\r\t")
	f.Add("ünïcode-hôst", "https://ü.example", "ключ")
	f.Add("---\n...", "|-\n>", "&a *a")

	f.Fuzz(func(t *testing.T, hostname, meshAddr, key string) {
		out, err := Render(Spec{Hostname: hostname, MeshAddr: meshAddr, AuthKey: key})
		if err != nil {
			if !errors.Is(err, ErrSpec) {
				t.Fatalf("Render failed for a reason that is neither a missing fact nor mangled text: %v", err)
			}
			// The only legal refusals are a missing fact or a fact that is not text.
			// Both encoders would otherwise rewrite invalid UTF-8 as U+FFFD, which is
			// the transformation this error exists to prevent.
			complete := hostname != "" && meshAddr != "" && key != ""
			readable := utf8.ValidString(hostname) && utf8.ValidString(meshAddr) && utf8.ValidString(key)
			if complete && readable {
				t.Fatalf("Render refused a complete, valid spec: %q %q %q", hostname, meshAddr, key)
			}
			return
		}
		if hostname == "" || meshAddr == "" || key == "" {
			t.Fatalf("Render accepted an incomplete spec: %q %q %q", hostname, meshAddr, key)
		}
		if !utf8.ValidString(hostname) || !utf8.ValidString(meshAddr) || !utf8.ValidString(key) {
			t.Fatalf("Render accepted a spec that is not valid UTF-8: %q %q %q", hostname, meshAddr, key)
		}

		if !strings.HasPrefix(string(out), Header) {
			t.Fatalf("the marker is missing: %q", firstLine(string(out)))
		}

		var doc struct {
			Hostname string      `yaml:"hostname"`
			Files    []writeFile `yaml:"write_files"`
			RunCmd   []string    `yaml:"runcmd"`
		}
		if err := yaml.Unmarshal([]byte(strings.TrimPrefix(string(out), Header)), &doc); err != nil {
			t.Fatalf("the rendering is not valid YAML: %v\n%s", err, out)
		}

		// The whole point: no input can become structure.
		if doc.Hostname != hostname {
			t.Fatalf("hostname %q rendered as %q", hostname, doc.Hostname)
		}
		if len(doc.Files) != 1 {
			t.Fatalf("%d files, want 1: an input created structure", len(doc.Files))
		}
		if len(doc.RunCmd) != 1 {
			t.Fatalf("runcmd = %v, want exactly the one command", doc.RunCmd)
		}

		var facts joinFile
		if err := json.Unmarshal([]byte(doc.Files[0].Content), &facts); err != nil {
			t.Fatalf("the join file is not JSON: %v\n%q", err, doc.Files[0].Content)
		}
		if facts.AuthKey != key || facts.MeshAddr != meshAddr || facts.Hostname != hostname {
			t.Fatalf("facts %+v do not match the spec %q %q %q", facts, hostname, meshAddr, key)
		}
	})
}
