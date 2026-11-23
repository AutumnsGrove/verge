package firefly

import (
	"strconv"
	"strings"
)

// Machine names.
//
// The network is keyed on a machine's name rather than its provider id, so the
// name is not cosmetic: it is the only identifier a tailnet can be asked about,
// and the wrong one means a machine that is running, billing, and invisible to
// both halves of the readiness rule.
//
// Firefly therefore mints it, at provisioning time, before Create — see
// docs/firefly.md, "What a node is called". Two properties matter and both come
// from the shape rather than from care:
//
//   - Uniqueness within the document, from a counter that is stored with it.
//   - Valid as a hostname, because the provider requires one and a name it
//     rejects is a create that fails after the name was chosen.

const (
	// maxNamePrefix bounds the prefix so that it, a separator, and the widest
	// counter (20 digits, a uint64) still fit the 63 characters a hostname may
	// occupy: 42 + 1 + 20 = 63. The provider validates the name as a hostname, and a
	// name it rejects is a create that fails after the name was chosen.
	maxNamePrefix = 42

	// fallbackNamePrefix is used when the label sanitises to nothing, which
	// cannot happen for a valid label but can for one made entirely of characters
	// a hostname does not allow.
	fallbackNamePrefix = "verge"
)

// namePrefixFor derives the name prefix from the deployment's label.
//
// The label already identifies the deployment -- it is what tells firefly's
// machines from someone else's in a project listing -- so the name carries it
// too, and a node called "verge-prod-12" says which deployment it belongs to in
// a tailnet that holds more than one. A single constant prefix would do the same
// job for one deployment and quietly collide for two: a tailnet's device names
// are unique per tailnet, not per project.
func namePrefixFor(labelKey, labelValue string) string {
	raw := strings.ToLower(labelKey + "-" + labelValue)

	var b strings.Builder
	lastHyphen := true // so a leading separator is not written
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}

	prefix := strings.Trim(b.String(), "-")
	if len(prefix) > maxNamePrefix {
		prefix = strings.Trim(prefix[:maxNamePrefix], "-")
	}
	if prefix == "" {
		return fallbackNamePrefix
	}
	return prefix
}

// mintName returns the next free name for a machine in doc, advancing the
// document's name counter as it goes.
//
// It takes the document rather than the firefly's own state because the counter
// and the machines it must not collide with live in the same place, and a name
// that collides is not a cosmetic bug: the document would be rejected by Validate
// and two machines would share one node record.
func (f *Firefly) mintName(doc *Document) string {
	for {
		doc.NextMachineNumber++
		name := f.namePrefix + "-" + strconv.FormatUint(doc.NextMachineNumber, 10)
		if !doc.nameTaken(name) {
			return name
		}
	}
}
