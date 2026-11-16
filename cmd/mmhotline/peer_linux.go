//go:build linux

package main

import "golang.org/x/sys/unix"

func commandPeerUID(socketFileDescriptor int) (uint32, error) {
	credentials, err := unix.GetsockoptUcred(socketFileDescriptor, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return credentials.Uid, nil
}
