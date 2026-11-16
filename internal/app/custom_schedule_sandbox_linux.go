//go:build linux

package app

import (
	"os"
	"runtime"
	"syscall"
	"time"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"golang.org/x/sys/unix"
)

// customScheduleSandboxProcessAttributes gives the child its own process
// group, so the parent can kill everything it started, and kills the child
// when the parent dies. Strictly, Pdeathsig fires when the parent thread that
// started the child exits; Go keeps its threads while the service runs, so
// in practice that is when the service ends.
func customScheduleSandboxProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// limitCustomScheduleSandboxResources runs first in the child: low CPU
// priority for every thread of the child's process group, then the resource
// limits.
func limitCustomScheduleSandboxResources() error {
	if err := syscall.Setpriority(syscall.PRIO_PGRP, 0, 19); err != nil {
		return err
	}
	limits := []struct {
		resource int
		value    syscall.Rlimit
	}{
		{syscall.RLIMIT_DATA, syscall.Rlimit{Cur: customScheduleDataLimitBytes, Max: customScheduleDataLimitBytes}},
		{syscall.RLIMIT_CPU, syscall.Rlimit{Cur: customScheduleRunCPUSecondsLimit, Max: customScheduleRunCPUSecondsLimit + 1}},
		{syscall.RLIMIT_FSIZE, syscall.Rlimit{Cur: 0, Max: 0}},
		{syscall.RLIMIT_CORE, syscall.Rlimit{Cur: 0, Max: 0}},
		{syscall.RLIMIT_NOFILE, syscall.Rlimit{Cur: customScheduleOpenFilesLimit, Max: customScheduleOpenFilesLimit}},
	}
	for _, limit := range limits {
		value := limit.value
		if err := syscall.Setrlimit(limit.resource, &value); err != nil {
			return err
		}
	}
	return nil
}

// customScheduleAllowedSystemCalls is what the Go runtime needs to run
// Starlark, write standard output, and exit. Anything else, such as opening a
// file, opening a socket, or starting a program, kills the process. The list
// is checked on the Linux test servers by the lockdown test for every Go upgrade.
//
// The list depends on the child calling runtime.GOMAXPROCS(1) first: that
// also stops the runtime from re-reading the cgroup CPU limit (Go 1.25 and
// later), which would need pread and kill the child.
var customScheduleAllowedSystemCalls = []string{
	"write", "exit", "exit_group",
	"munmap", "madvise", "brk",
	"futex", "sched_yield", "nanosleep", "clock_gettime", "clock_nanosleep",
	"rt_sigreturn", "rt_sigaction", "rt_sigprocmask", "sigaltstack",
	"gettid", "getpid", "restart_syscall",
	"set_robust_list", "rseq", "sched_getaffinity", "getrandom",
	// The runtime's timers (the garbage collector's scavenger) wait in
	// epoll and wake it through an eventfd, both created before the lockdown.
	"epoll_pwait", "read",
}

// lockDownCustomScheduleSandbox sets no_new_privs and installs the seccomp
// allowlist on every thread. If it fails, the run fails: the sandbox never
// runs customer code without seccomp on Linux.
func lockDownCustomScheduleSandbox() error {
	// Start the runtime's network poller, which timers need, while it may
	// still create its epoll and eventfd descriptors.
	time.AfterFunc(time.Hour, func() {}).Stop()
	// no_new_privs belongs to one thread, and the kernel refuses the filter
	// (EACCES) from a thread without it. LoadFilter sets it and then loads
	// the filter in two system calls, between which Go may move a goroutine
	// to another thread; locking keeps both on one. FilterFlagTSync then gives
	// every thread the filter and no_new_privs.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return seccomp.LoadFilter(seccomp.Filter{
		NoNewPrivs: true,
		Flag:       seccomp.FilterFlagTSync,
		Policy: seccomp.Policy{
			DefaultAction: seccomp.ActionKillProcess,
			Syscalls: []seccomp.SyscallGroup{
				{Action: seccomp.ActionAllow, Names: customScheduleAllowedSystemCalls},
				{Action: seccomp.ActionAllow, NamesWithCondtions: []seccomp.NameWithConditions{
					// New threads for the Go runtime only: clone must share
					// the address space as a thread (CLONE_THREAD), never fork.
					{Name: "clone", Conditions: seccomp.ArgumentConditions{{Argument: 0, Operation: seccomp.BitsSet, Value: syscall.CLONE_THREAD}}},
					// Memory, but never executable memory: the Go runtime
					// needs none.
					{Name: "mmap", Conditions: seccomp.ArgumentConditions{{Argument: 2, Operation: seccomp.BitsNotSet, Value: syscall.PROT_EXEC}}},
					{Name: "mprotect", Conditions: seccomp.ArgumentConditions{{Argument: 2, Operation: seccomp.BitsNotSet, Value: syscall.PROT_EXEC}}},
					// Signals to the child's own threads only (the runtime
					// preempts goroutines this way), never to another process.
					{Name: "tgkill", Conditions: seccomp.ArgumentConditions{{Argument: 0, Operation: seccomp.Equal, Value: uint64(os.Getpid())}}},
					// Names for the runtime's own memory, such as
					// "[anon: Go: heap]": Go 1.25 and later name each new
					// mapping on kernels that support it (Ubuntu 26.04's
					// does), and nothing else of prctl.
					{Name: "prctl", Conditions: seccomp.ArgumentConditions{
						{Argument: 0, Operation: seccomp.Equal, Value: unix.PR_SET_VMA},
						{Argument: 1, Operation: seccomp.Equal, Value: unix.PR_SET_VMA_ANON_NAME},
					}},
				}},
			},
		},
	})
}
