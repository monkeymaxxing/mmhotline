//go:build darwin || freebsd

package main

import "golang.org/x/sys/unix"

func commandPeerUID(socketFileDescriptor int) (uint32, error) {
	credentials, err := unix.GetsockoptXucred(socketFileDescriptor, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return credentials.Uid, nil
}
