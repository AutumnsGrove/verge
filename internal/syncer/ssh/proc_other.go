//go:build !unix

package ssh

import "os/exec"

// configure is a no-op where process groups are not a thing; the command is killed on its
// own, which is all this platform offers.
func configure(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
