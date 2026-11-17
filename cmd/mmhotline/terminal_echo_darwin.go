//go:build darwin

package main

import "golang.org/x/sys/unix"

const (
	terminalGetRequest = unix.TIOCGETA
	terminalSetRequest = unix.TIOCSETA
)
