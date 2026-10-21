//go:build !(linux || darwin)

package dbx

import "net"

// socketIdle cannot peek a socket on this platform, so the acquire check
// hands every connection out unchecked.
func socketIdle(net.Conn) (idle, known bool) { return false, false }
