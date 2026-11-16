package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// Called before starting background work: umask is process-wide.
func listenUnixSocket(path string) (net.Listener, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket file %s", path)
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	previous := unix.Umask(0177)
	defer unix.Umask(previous)
	return net.Listen("unix", path)
}

// notifySystemdReady tells systemd that serve accepts connections, so that
// systemctl start and restart return only then (Type=notify) and a command
// right after them reaches the socket. Outside systemd NOTIFY_SOCKET is unset.
// Go reads a leading @ in the address as Linux's abstract socket namespace.
func notifySystemdReady() error {
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return nil
	}
	connection, err := net.Dial("unixgram", address)
	if err != nil {
		return fmt.Errorf("tell systemd that mmhotline serve is ready: %w", err)
	}
	defer connection.Close()
	if _, err = connection.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("tell systemd that mmhotline serve is ready: %w", err)
	}
	return nil
}

func authorizeCommandPeer(peerUID, serverUID uint32, operatingSystem string) error {
	allowedUID := uint32(0)
	if operatingSystem != "linux" {
		allowedUID = serverUID
	}
	if peerUID != allowedUID {
		return errors.New("Run the mmhotline command with sudo.")
	}
	return nil
}

type commandPeerContextKey struct{}
type commandPeer struct{ err error }

func commandConnectionContext(ctx context.Context, connection net.Conn) context.Context {
	var peerUID uint32
	var credentialError error
	socket, ok := connection.(syscall.Conn)
	if !ok {
		return context.WithValue(ctx, commandPeerContextKey{}, commandPeer{errors.New("cannot read the Unix user credentials of the command socket connection")})
	}
	raw, err := socket.SyscallConn()
	if err == nil {
		err = raw.Control(func(descriptor uintptr) { peerUID, credentialError = commandPeerUID(int(descriptor)) })
	}
	if err == nil {
		err = credentialError
	}
	if err == nil {
		err = authorizeCommandPeer(peerUID, uint32(os.Geteuid()), runtime.GOOS)
	}
	return context.WithValue(ctx, commandPeerContextKey{}, commandPeer{err})
}
func checkCommandPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := r.Context().Value(commandPeerContextKey{}).(commandPeer)
		if !ok || peer.err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			// A failed write means the command went away; nothing is left to do.
			_ = json.NewEncoder(w).Encode(struct {
				Message string `json:"message"`
			}{"Run the mmhotline command with sudo."})
			return
		}
		next.ServeHTTP(w, r)
	})
}
