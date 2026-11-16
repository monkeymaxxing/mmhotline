package triageagent

import (
	"context"
	"io"
	"net"
)

// listenForProxy forwards each connection to MMHotline's logging proxy at
// socketPath, after a first line with the run's proxy token, by which the
// proxy knows the run and its hosts.
func listenForProxy(ctx context.Context, socketPath, proxyToken string) (net.Listener, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go forwardProxyConnection(ctx, connection, socketPath, proxyToken)
		}
	}()
	return listener, nil
}

func forwardProxyConnection(ctx context.Context, connection net.Conn, socketPath, proxyToken string) {
	defer connection.Close()
	stopClosing := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClosing()
	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return
	}
	defer upstream.Close()
	if _, err = io.WriteString(upstream, proxyToken+"\n"); err != nil {
		return
	}
	stopClosingUpstream := context.AfterFunc(ctx, func() { _ = upstream.Close() })
	defer stopClosingUpstream()
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, connection)
		_ = upstream.(*net.UnixConn).CloseWrite()
		close(copied)
	}()
	_, _ = io.Copy(connection, upstream)
	_ = connection.(*net.TCPConn).CloseWrite()
	<-copied
}
