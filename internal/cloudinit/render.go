// Package cloudinit renders the boot script a machine runs as root.
//
// It exists to carry exactly three facts into a machine: its name, the mesh it
// belongs to, and a single-use enrolment key for that mesh. Nothing else — not a
// repository, not a model, not a provider credential. A boot script is the most
// privileged thing this system writes, so it stays small enough to read in full and
// its inputs are the only ones the design promises a machine gets (docs/firefly.md,
// "What firefly depends on").
//
// The name is load-bearing rather than cosmetic. `Network` is keyed on the machine's
// name (docs/firefly.md, "What a node is called"), and the tailnet client matches a
// device by the hostname it joined under — so a machine that boots with the image's
// default hostname is a machine nothing can ask about, which fails as a machine that
// never becomes READY rather than as a bad boot script. Setting it here is what makes
// that match true.
//
// There is no text template. The document is a struct marshalled by a YAML encoder,
// because the alternative — interpolating strings into a template — is a parser on
// both sides of a trust boundary, and the fuzz target exists to prove the encoder's
// escaping rather than a template's quoting.
//
// Every string in the document is emitted double-quoted, and that is not decoration.
// Left to itself the encoder writes a multi-line string as a block scalar, which is
// correct for most values and lossy for some: a fact that is exactly "\n" renders as
// an empty scalar, and one that starts with a newline loses it. A hostname that comes
// out of this package different from the one the network will be asked about is a
// machine nothing can find, so the ambiguity is removed rather than tolerated — the
// fuzzer found it, and every value now round-trips byte for byte or the renderer
// refuses it.
package cloudinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Path is where the join facts are written on the machine.
//
// A file rather than on the command line, because a command line is visible in `ps`
// to every process on the box, and because the agent reads it once and the key is
// single-use.
const Path = "/etc/verge/join.json"

// joinCommand is what the machine runs once the file is in place.
//
// The image provides it, and that is the boundary: this package decides *what* a
// machine needs to know, the image decides *how* to join a mesh. A renderer that
// installed anything would be a renderer that runs `curl | sh` as root at boot, which
// is the shape of mistake this package exists to avoid — the image is built and
// reviewed once, and nothing fetches code while booting.
const joinCommand = "/usr/local/bin/verge-join " + Path

// ErrSpec means the renderer was given a fact it cannot put on a machine.
//
// There are two of those, and the second is not obvious: a fact that is missing, and a
// fact that is not valid UTF-8. Both are refused rather than rendered, because both
// encoders this package uses — JSON for the join file, YAML for the document — rewrite
// invalid UTF-8 as U+FFFD instead of failing. A silently mangled enrolment key is a
// machine that boots, cannot join, and reports nothing about why; a name that comes out
// of the script different from the one the network will be asked about is a machine that
// never becomes READY. Refusing is the only version of this that cannot look like
// success.
var ErrSpec = errors.New("cloudinit: incomplete spec")

// Spec is what a machine is told at boot.
type Spec struct {
	// Hostname is the machine's name, and it has to be the one firefly minted: the
	// network is later asked about exactly this string.
	Hostname string

	// MeshAddr is where the mesh agent finds its control plane. It is not a secret.
	MeshAddr string

	// AuthKey is a single-use, short-lived enrolment key. It grants membership of the
	// private network and nothing else, which is why it may go on a machine at all —
	// a credential that can spend money never does (docs/firefly.md).
	AuthKey string
}

// joinFile is the document written to Path.
//
// JSON inside the YAML rather than YAML inside the YAML: the agent parses this, and a
// format with exactly one encoding is one fewer thing to get wrong at 3am.
type joinFile struct {
	Hostname string `json:"hostname"`
	MeshAddr string `json:"meshAddr"`
	AuthKey  string `json:"authKey"`
}

// quoted is a string the encoder must emit as a double-quoted scalar.
//
// It exists because the default scalar style is chosen by the encoder and, for a value
// that begins or ends with whitespace, is not reversible. Quoting is always reversible.
type quoted string

// MarshalYAML pins the style. The tag is set as well as the style so the value is a
// string even when it would otherwise look like a number or a bool ("0600", "true").
func (q quoted) MarshalYAML() (any, error) {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Style: yaml.DoubleQuotedStyle,
		Value: string(q),
	}, nil
}

// writeFile is one cloud-init write_files entry.
type writeFile struct {
	Path        quoted `yaml:"path"`
	Owner       quoted `yaml:"owner"`
	Permissions quoted `yaml:"permissions"`
	Content     quoted `yaml:"content"`
}

// config is the cloud-config document. The field tags are the cloud-init keys; the
// set is deliberately tiny.
type config struct {
	// Hostname is set explicitly, and preserve_hostname keeps the distribution from
	// overwriting it from DHCP: a machine whose hostname feeds a tailnet record must
	// not have it change after the node joins.
	Hostname         quoted      `yaml:"hostname"`
	PreserveHostname bool        `yaml:"preserve_hostname"`
	WriteFiles       []writeFile `yaml:"write_files"`
	RunCmd           []quoted    `yaml:"runcmd"`
}

// Header is the first line cloud-init requires. It is separate from the document
// because it is a marker, not data.
const Header = "#cloud-config\n"

// Render returns the boot script for s.
//
// Every input goes through an encoder, so no byte of any of them can leave the value
// it was put in: a newline in a name becomes an escaped byte inside a quoted scalar,
// not a new key in the document. That is the whole security argument, and FuzzCloudInit
// is what holds it.
func Render(s Spec) ([]byte, error) {
	switch {
	case s.Hostname == "":
		return nil, fmt.Errorf("%w: no hostname", ErrSpec)
	case !utf8.ValidString(s.Hostname):
		return nil, fmt.Errorf("%w: the hostname is not valid UTF-8", ErrSpec)
	case s.MeshAddr == "":
		return nil, fmt.Errorf("%w: no mesh address", ErrSpec)
	case !utf8.ValidString(s.MeshAddr):
		return nil, fmt.Errorf("%w: the mesh address is not valid UTF-8", ErrSpec)
	case s.AuthKey == "":
		return nil, fmt.Errorf("%w: no enrolment key", ErrSpec)
	case !utf8.ValidString(s.AuthKey):
		return nil, fmt.Errorf("%w: the enrolment key is not valid UTF-8", ErrSpec)
	}

	// Spec and joinFile have the same fields in the same order, so the conversion is
	// the mapping: a field added to one without the other stops compiling.
	facts, err := json.Marshal(joinFile(s))
	if err != nil {
		// Only a struct of strings can fail here, which is to say a bug in this file.
		return nil, fmt.Errorf("cloudinit: encoding the join file: %w", err)
	}

	doc, err := yaml.Marshal(config{
		Hostname:         quoted(s.Hostname),
		PreserveHostname: true,
		WriteFiles: []writeFile{{
			Path:        quoted(Path),
			Owner:       quoted("root:root"),
			Permissions: quoted("0600"),
			Content:     quoted(facts),
		}},
		RunCmd: []quoted{quoted(joinCommand)},
	})
	if err != nil {
		return nil, fmt.Errorf("cloudinit: encoding the document: %w", err)
	}

	return append([]byte(Header), doc...), nil
}
