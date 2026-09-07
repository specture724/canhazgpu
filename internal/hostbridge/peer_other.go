//go:build !linux

package hostbridge

import (
	"fmt"
	"net"
)

func peerPID(conn *net.UnixConn) (int, error) {
	return 0, fmt.Errorf("Docker host bridge requires Linux")
}
