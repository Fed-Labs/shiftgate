package agent

import (
	"context"
	"errors"
	"net"
	"syscall"
)

type peerCredentials struct {
	PID int32
	UID uint32
	GID uint32
}

type credentialConn struct {
	net.Conn
	credentials peerCredentials
}

type credentialListener struct {
	net.Listener
}

func (l credentialListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return connection, nil
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	var credentials *syscall.Ucred
	var controlErr error
	if err := raw.Control(func(fileDescriptor uintptr) {
		credentials, controlErr = syscall.GetsockoptUcred(int(fileDescriptor), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if controlErr != nil || credentials == nil {
		_ = connection.Close()
		if controlErr != nil {
			return nil, controlErr
		}
		return nil, errors.New("unix peer credentials unavailable")
	}
	return &credentialConn{Conn: connection, credentials: peerCredentials{PID: credentials.Pid, UID: credentials.Uid, GID: credentials.Gid}}, nil
}

type credentialsContextKey struct{}

func connectionContext(ctx context.Context, connection net.Conn) context.Context {
	if peer, ok := connection.(*credentialConn); ok {
		return context.WithValue(ctx, credentialsContextKey{}, peer.credentials)
	}
	return ctx
}

func requestCredentials(ctx context.Context) (peerCredentials, bool) {
	value, ok := ctx.Value(credentialsContextKey{}).(peerCredentials)
	return value, ok
}
