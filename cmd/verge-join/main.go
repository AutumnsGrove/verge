// Command verge-join joins the machine's private network, using the three facts
// cloud-init wrote to /etc/verge/join.json: its name, the mesh's control plane, and a
// single-use enrolment key.
//
// It is the image's half of the renderer's contract (internal/cloudinit): the renderer
// decides what a machine needs to know, and this decides how a mesh is joined. The
// renderer names the command and the file; nothing fetches code at boot.
//
// The rules it keeps:
//
//   - **The key is never on a command line.** The join file exists so the key is not in
//     a cloud-init script or a `ps` line, and passing it as `--authkey=` would undo
//     that. It goes to the child process in its environment, which is readable by root
//     — the same root that can read the 0600 file.
//   - **Tailscale SSH is enabled at join time** (`--ssh`). The control plane reaches a
//     machine over ssh, so a machine that joined without it is a machine that boots,
//     becomes READY, and then cannot be saved. Enabling it here is what makes the
//     transport's assumption true rather than hoped for.
//   - **Joining is verified, not assumed.** `tailscale up` returning zero means the CLI
//     accepted the request, not that the node is connected; the join polls
//     `tailscale status` until the backend reports Running, and fails cloud-init if it
//     never does. A machine that boots and silently never joins looks exactly like a
//     machine that never booted, and that ambiguity costs a boot deadline to discover.
//   - **The hostname is passed explicitly.** The tailnet matches devices by hostname,
//     and firefly asks the network about the name it minted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// joinFile is what internal/cloudinit writes, and the field names are that package's
// contract rather than this one's: it renders the JSON.
type joinFile struct {
	Hostname string `json:"hostname"`
	MeshAddr string `json:"meshAddr"`
	AuthKey  string `json:"authKey"`
}

// errJoinFile means the file is missing, unreadable, or does not carry the facts.
var errJoinFile = errors.New("verge-join: unusable join file")

// cli is what runs the tailnet's command. exec.CommandContext is the real one, and a
// test supplies its own: the seam is "the tailscale CLI", and every assertion worth
// making here is about what it is asked.
type cli func(ctx context.Context, name string, env []string, args ...string) ([]byte, error)

// options are the knobs a test moves; the defaults are the daemon's.
type options struct {
	// binary is the tailscale CLI.
	binary string
	// timeout bounds the poll for a connected node, and the CLI's own attempt.
	timeout time.Duration
	// sleep is how the poll waits. Injected so the tests do not.
	sleep func(time.Duration)
	// log is where progress goes. Never the key.
	log io.Writer
}

func defaults() options {
	return options{
		binary:  "tailscale",
		timeout: 5 * time.Minute,
		sleep:   time.Sleep,
		log:     os.Stderr,
	}
}

func main() {
	path := "/etc/verge/join.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	o := defaults()
	if err := join(ctx, path, o, execCLI(o)); err != nil {
		fmt.Fprintln(os.Stderr, "verge-join:", err)
		os.Exit(1)
	}
}

// execCLI runs the real binary.
func execCLI(o options) cli {
	return func(ctx context.Context, name string, env []string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, o.binary, args...)
		cmd.Env = append(os.Environ(), env...)
		var out, errb strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			// The CLI's stderr is the diagnosis; a bare "exit status 1" is not.
			if msg := strings.TrimSpace(errb.String()); msg != "" {
				return nil, fmt.Errorf("%w: %s", err, msg)
			}
			return nil, err
		}
		return []byte(out.String()), nil
	}
}

// join reads the facts, asks the mesh for membership, and waits until it is real.
func join(ctx context.Context, path string, o options, run cli) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errJoinFile, path, err)
	}
	var facts joinFile
	if err := json.Unmarshal(raw, &facts); err != nil {
		return fmt.Errorf("%w: %s is not JSON: %w", errJoinFile, path, err)
	}
	if strings.TrimSpace(facts.Hostname) == "" {
		return fmt.Errorf("%w: no hostname", errJoinFile)
	}
	if strings.TrimSpace(facts.AuthKey) == "" {
		return fmt.Errorf("%w: no enrolment key", errJoinFile)
	}
	hostname, err := host(facts.Hostname)
	if err != nil {
		return err
	}

	up := []string{
		"--hostname=" + hostname,
		// The control plane saves over ssh, so the node must serve it.
		"--ssh",
		// A machine's DNS is the image's business; a mesh that rewrites resolv.conf on
		// a box whose whole job is reaching the internet is a box that cannot resolve
		// its own package mirrors.
		"--accept-dns=false",
		"--timeout=2m",
	}
	if addr := strings.TrimSpace(facts.MeshAddr); addr != "" {
		up = append(up, "--login-server="+addr)
	}

	if _, err := run(ctx, o.binary, []string{"TS_AUTHKEY=" + facts.AuthKey}, append([]string{"up"}, up...)...); err != nil {
		return fmt.Errorf("joining the mesh: %w", err)
	}

	return waitForRunning(ctx, run, o, hostname)
}

// waitForRunning polls the CLI until the backend reports a connected node.
func waitForRunning(ctx context.Context, run cli, o options, hostname string) error {
	deadline := time.Now().Add(o.timeout)
	for {
		out, err := run(ctx, o.binary, nil, "status", "--json")
		if err == nil && running(out) {
			fmt.Fprintf(o.log, "verge-join: joined as %s\n", hostname)
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("the mesh did not report a connected node within %s: %w", o.timeout, err)
			}
			return fmt.Errorf("the mesh did not report a connected node within %s", o.timeout)
		}
		o.sleep(2 * time.Second)
	}
}

// running reports whether `tailscale status --json` says the node is connected.
//
// Only one field is read, because only one is a fact this program needs, and a struct
// that grew to match the CLI's whole status would be a dependency on its schema rather
// than on the one word it is asked for.
func running(statusJSON []byte) bool {
	var status struct {
		BackendState string `json:"BackendState"`
	}
	if err := json.Unmarshal(statusJSON, &status); err != nil {
		return false
	}
	return status.BackendState == "Running"
}

// host checks the name can be a hostname before it becomes one.
//
// The name is minted by firefly and rendered into YAML by cloudinit, so this is the
// third gate rather than the first -- but it is also what keeps a name with a space in
// it from arriving as two arguments, and it costs a few lines.
func host(name string) (string, error) {
	if name == "" {
		// The caller checks for a blank hostname before this, and the validator still
		// refuses one: a validator that only rejects what its current caller happens to
		// pass is a validator with an unstated precondition. The fuzzer found this.
		return "", fmt.Errorf("%w: hostname is empty", errJoinFile)
	}
	if len(name) > 253 {
		return "", fmt.Errorf("%w: hostname is too long", errJoinFile)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return "", fmt.Errorf("%w: hostname %q has a character a hostname cannot have", errJoinFile, name)
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%w: hostname %q cannot begin with %q", errJoinFile, name, name[0])
	}
	return name, nil
}
