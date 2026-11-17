//go:build linux

package main

import "golang.org/x/sys/unix"

const (
	terminalGetRequest = unix.TCGETS
	terminalSetRequest = unix.TCSETS
)
