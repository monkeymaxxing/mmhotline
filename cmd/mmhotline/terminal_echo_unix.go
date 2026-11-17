//go:build linux || darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// turnOffTerminalEcho stops the terminal file from showing what a person
// types. It returns a function that puts the terminal back, and false when
// file is not a terminal, such as a pipe.
func turnOffTerminalEcho(file *os.File) (restore func(), isTerminal bool) {
	descriptor := int(file.Fd())
	original, err := unix.IoctlGetTermios(descriptor, terminalGetRequest)
	if err != nil {
		return nil, false
	}
	silent := *original
	silent.Lflag &^= unix.ECHO | unix.ECHONL
	if err := unix.IoctlSetTermios(descriptor, terminalSetRequest, &silent); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(descriptor, terminalSetRequest, original) }, true
}
