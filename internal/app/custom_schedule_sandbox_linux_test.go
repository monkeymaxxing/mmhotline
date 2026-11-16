//go:build linux

package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestCustomScheduleLockdownKillsForbiddenSystemCalls proves that after the
// sandbox child's lockdown, opening a file, opening a socket, and starting a
// program each kill the process. Run it on the Linux test servers for every Go upgrade: the Go
// runtime's own system calls can change.
func TestCustomScheduleLockdownKillsForbiddenSystemCalls(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := func(kind string) (*exec.ExitError, string) {
		command := exec.Command(executable, "custom-schedule-sandbox-lockdown-probe", kind)
		command.Env = []string{}
		command.Dir = "/"
		command.SysProcAttr = customScheduleSandboxProcessAttributes()
		var stderr bytes.Buffer
		command.Stderr = &stderr
		err := command.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return exit, stderr.String()
	}
	for _, kind := range []string{"nothing", "runtime"} {
		if exit, stderr := probe(kind); exit != nil {
			t.Fatalf("%s: the locked-down child did not survive: %v %s", kind, exit, stderr)
		}
	}
	for _, kind := range []string{"open", "socket", "exec", "executable-memory", "signal-parent"} {
		exit, stderr := probe(kind)
		if exit == nil {
			t.Fatalf("%s did not kill the child", kind)
		}
		status := exit.Sys().(syscall.WaitStatus)
		if !status.Signaled() || status.Signal() != syscall.SIGSYS {
			t.Fatalf("%s: child ended with %v, want SIGSYS: %s", kind, exit, stderr)
		}
	}
}

// customScheduleLockdownProbe locks itself down exactly like the sandbox
// child, then tries one forbidden system call, or ("runtime") works the Go
// runtime: goroutines, timers, and garbage collection. Surviving it exits 0.
func customScheduleLockdownProbe(kind string) int {
	runtime.GOMAXPROCS(1)
	if err := limitCustomScheduleSandboxResources(); err != nil {
		return customScheduleSandboxExitLockdownFailed
	}
	if err := lockDownCustomScheduleSandbox(); err != nil {
		return customScheduleSandboxExitLockdownFailed
	}
	switch kind {
	case "open":
		_, _ = syscall.Open("/etc/hostname", syscall.O_RDONLY, 0)
	case "socket":
		_, _ = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	case "exec":
		_ = syscall.Exec("/bin/true", []string{"true"}, nil)
	case "executable-memory":
		_, _ = syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_EXEC, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	case "signal-parent":
		_ = syscall.Tgkill(os.Getppid(), os.Getppid(), 0)
	case "runtime":
		var garbage [][]byte
		done := make(chan bool)
		for range 8 {
			go func() {
				time.Sleep(20 * time.Millisecond)
				done <- true
			}()
		}
		for range 2000 {
			garbage = append(garbage, make([]byte, 64*1024))
			if len(garbage) > 100 {
				garbage = garbage[:0]
			}
		}
		runtime.GC()
		for range 8 {
			<-done
		}
	}
	return 0
}
