//go:build !linux && !darwin

package main

import "os"

// turnOffTerminalEcho does nothing on systems the command does not support
// for terminal input; a secret then reads from standard input as from a pipe.
func turnOffTerminalEcho(*os.File) (restore func(), isTerminal bool) { return nil, false }

// fileIsTerminal is false on systems the command does not support for
// terminal input, so a new signing secret is shown only with --write-to-file.
func fileIsTerminal(*os.File) bool { return false }
