//go:build linux || darwin

package dbx

import (
	"errors"
	"net"
	"syscall"
)

// socketIdle reports whether conn's socket has nothing waiting to be read and
// is still open, without blocking and without consuming a byte (MSG_PEEK on
// the non-blocking descriptor Go already uses). known is false when the socket
// cannot be reached, so the caller must not draw a conclusion. A TLS
// connection is checked on its TCP socket: a record waiting there means the
// server sent something.
func socketIdle(conn net.Conn) (idle, known bool) {
	if tc, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = tc.NetConn()
	}
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false, false
	}
	var peekErr error
	var buf [1]byte
	if err := raw.Read(func(fd uintptr) bool {
		_, _, peekErr = syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK)
		return true
	}); err != nil {
		return false, false
	}
	// EAGAIN: nothing to read, still open. Data, EOF (n == 0) or a socket
	// error all mean the connection is not simply idle.
	return errors.Is(peekErr, syscall.EAGAIN), true
}
