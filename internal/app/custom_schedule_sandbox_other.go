//go:build !linux

package app

import "syscall"

// On macOS, which is for development only, the sandbox child runs without
// seccomp and without a parent-death signal. Production runs on Linux, where
// the lockdown is complete (custom_schedule_sandbox_linux.go).

func customScheduleSandboxProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// macOS refuses a 256 MiB RLIMIT_DATA for a Go process and would not enforce
// it anyway, so "too much memory" is detected only on Linux.
func limitCustomScheduleSandboxResources() error {
	if err := syscall.Setpriority(syscall.PRIO_PGRP, 0, 19); err != nil {
		return err
	}
	for resource, value := range map[int]syscall.Rlimit{
		syscall.RLIMIT_CPU:    {Cur: customScheduleRunCPUSecondsLimit, Max: customScheduleRunCPUSecondsLimit + 1},
		syscall.RLIMIT_FSIZE:  {Cur: 0, Max: 0},
		syscall.RLIMIT_CORE:   {Cur: 0, Max: 0},
		syscall.RLIMIT_NOFILE: {Cur: customScheduleOpenFilesLimit, Max: customScheduleOpenFilesLimit},
	} {
		if err := syscall.Setrlimit(resource, &value); err != nil {
			return err
		}
	}
	return nil
}

// lockDownCustomScheduleSandbox does nothing on macOS: there is no seccomp.
func lockDownCustomScheduleSandbox() error { return nil }
