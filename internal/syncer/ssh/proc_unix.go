//go:build unix

package ssh

import (
	"os/exec"
	"syscall"
)

// configure makes the command the leader of its own process group.
//
// Without this, cancelling a save kills the command and leaves anything it started
// holding the pipe open: the reader waits for output nobody is going to write, which is
// exactly the hang the Fetcher contract says cannot happen. It is also why the ssh
// process is not in our group -- killing the group means killing what it started.
func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminate kills the command's whole process group.
func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// The negative pid is the group: configure made the child a group leader, so its pid
	// is the group id.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// A process that already exited is not a failure to stop it.
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	return nil
}
