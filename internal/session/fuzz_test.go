package session

// This file is in-package because the property is about validID, and a test in another
// package would have to re-implement it to check it.

import (
	"regexp"
	"strings"
	"testing"
)

// idPattern is the alphabet the session's command assumes, written independently of
// validID: a property that asked validID whether validID was right would prove nothing.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// FuzzValidID is this package's security target. The id is interpolated, unquoted, into one
// command string that runs over ssh -- so validID is the entire defence between a hostile
// id and a remote shell, and "the fixed list of bad ids is refused" is not a defence.
//
// Two properties over every input:
//
//   - an accepted id is inside the alphabet the command is written for, and inside the
//     length limit;
//   - Command of an accepted id is exactly the template with that id in it, so no quoting
//     layer can start treating part of the id as syntax. A future edit that shell-quoted
//     the value would break this, and the image's contract with it.
//
// A rejected id is not Command's business: nothing may call it, which is what TestOpen's
// bad case asserts.
func FuzzValidID(f *testing.F) {
	for _, seed := range []string{
		"s-one", "abc", "a", "a_b-1", "01j8xyz",
		"", " ", ".", "..", "../x", "a/b", "-x", "UPPER", "a.b",
		"a\nb", "a;rm -rf /", "$(id)", "`id`", "a'b", `a"b`, "a&b", "a|b", "a b",
		strings.Repeat("a", maxID), strings.Repeat("a", maxID+1),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, id string) {
		if !validID(id) {
			return
		}
		if !idPattern.MatchString(id) {
			t.Fatalf("validID accepted %q, which is outside %s", id, idPattern)
		}
		if len(id) > maxID {
			t.Fatalf("validID accepted a %d-byte id, over the %d-byte limit", len(id), maxID)
		}

		want := "mkdir -p /workspace/" + id + " && cd /workspace/" + id +
			" && exec pi --mode rpc --session-dir /workspace/" + id + "/.pi/sessions --session-id " + id
		if got := Command(id); got != want {
			t.Fatalf("Command(%q) = %q, want %q", id, got, want)
		}
	})
}
