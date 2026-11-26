package main

import (
	"strings"
	"testing"
)

// FuzzHost pins the property the validator exists for rather than a list of names it
// happens to reject: an accepted hostname is one argv-safe token, passed to the CLI
// exactly as the network minted it.
//
// The interesting inputs are the ones that turn into syntax -- a leading dash becomes a
// flag, a space becomes two arguments, a slash becomes a path -- because this value
// comes from a JSON file that a boot script wrote, and the CLI it is handed to treats
// every one of those as meaning something.
func FuzzHost(f *testing.F) {
	for _, seed := range []string{
		"verge-machine-1",
		"a",
		"a.b-c_d",
		"-x",
		".hidden",
		"two words",
		"a/b",
		"..",
		"",
		strings.Repeat("a", 300),
		"a\nb",
		"a\x00b",
		"ünïcode",
		"a\tb",
		"--hostname=other",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got, err := host(name)
		if err != nil {
			return
		}

		if got != name {
			t.Fatalf("host(%q) = %q: the name must reach the network unchanged", name, got)
		}
		if got == "" || len(got) > 253 {
			t.Fatalf("host(%q) accepted a name of length %d", name, len(got))
		}
		if strings.ContainsAny(got, " \t\n\r\x00/\\") {
			t.Fatalf("host(%q) accepted a name with whitespace, a NUL, or a separator", name)
		}
		if strings.HasPrefix(got, "-") || strings.HasPrefix(got, ".") {
			t.Fatalf("host(%q) accepted a name that reads as an option or a hidden path", name)
		}
		for _, r := range got {
			switch {
			case r >= 'a' && r <= 'z',
				r >= 'A' && r <= 'Z',
				r >= '0' && r <= '9',
				r == '-', r == '_', r == '.':
			default:
				t.Fatalf("host(%q) accepted %q, which is not a hostname character", name, r)
			}
		}
	})
}
