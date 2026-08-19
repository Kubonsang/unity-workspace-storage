//go:build linux

package workspaced

import (
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func authorizePeer(connection net.Conn) error {
	fd, err := rawFD(connection)
	if err != nil {
		return err
	}
	credentials, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return err
	}
	if int(credentials.Uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d does not match daemon uid", credentials.Uid)
	}
	return nil
}
