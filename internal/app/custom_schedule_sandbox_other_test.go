//go:build !linux

package app

// customScheduleLockdownProbe exists only on Linux, where the sandbox child
// locks itself down with seccomp.
func customScheduleLockdownProbe(string) int { return 0 }
