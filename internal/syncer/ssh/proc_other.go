//go:build !unix

// This file is what makes the package build on a machine that is not Linux or macOS.
//
// It is not a supported deployment path: the image and the control plane are both Linux,
// and the behaviour offered here is the one proc_unix.go explains is wrong -- killing the
// command and leaving anything it started holding the pipe. It exists so a developer on
// another platform can run `go build`, and it is deliberately not tested, because a test
// for it would be a test that a platform nobody deploys to behaves like a degraded
// version of the one that is.
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
