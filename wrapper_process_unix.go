//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureWrapperProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopCredentialWrapper(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	pgid, groupErr := syscall.Getpgid(command.Process.Pid)
	if groupErr == nil {
		_ = syscall.Kill(-pgid, syscall.SIGINT)
	} else {
		_ = command.Process.Signal(os.Interrupt)
	}
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if groupErr == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = command.Process.Kill()
	}
}
