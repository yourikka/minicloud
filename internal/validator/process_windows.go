//go:build windows

package validator

import "os/exec"

func configureProcess(*exec.Cmd) {}

func killProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
