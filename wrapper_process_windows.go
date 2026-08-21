//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

func configureWrapperProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func stopCredentialWrapper(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	// Wrapper may create child processes, so ask Windows to terminate the
	// complete process tree. Fall back to the direct child if taskkill fails.
	err := exec.Command("taskkill", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F").Run()
	if err != nil {
		_ = command.Process.Kill()
	}
}
