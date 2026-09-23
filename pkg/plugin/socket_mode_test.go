// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A UNIX socket's mode is 0777 &^ umask, so the test sets umask 0: under the usual
// 0022 the code before #687 passed as well.
func TestListen_SocketIsOwnerOnlyUnderAPermissiveUmask(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "net-dhcp.sock")

	old := syscall.Umask(0)
	defer syscall.Umask(old)

	p := &Plugin{}
	errCh := make(chan error, 1)
	go func() { errCh <- p.Listen(sockPath) }()
	defer func() {
		_ = p.server.Close()
		<-errCh
	}()

	deadline := time.Now().Add(5 * time.Second)
	var fi os.FileInfo
	for {
		var err error
		fi, err = os.Stat(sockPath)
		if err == nil {
			break
		}
		select {
		case lerr := <-errCh:
			t.Fatalf("Listen returned before the socket appeared: %v", lerr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s never appeared", sockPath)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("plugin socket mode is %#o, want no group or other bits: "+
			"the RPC surface is reachable by more than the owner", mode)
	}

	c, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("owner cannot connect to the restricted socket: %v", err)
	}
	_ = c.Close()
}
