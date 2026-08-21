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

// TestListen_SocketIsOwnerOnlyUnderAPermissiveUmask pins the property
// SECURITY.md relies on when it calls serving /metrics on the plugin
// socket "unchanged ground": that socket is root-only, so anything
// able to read it can already call every RPC.
//
// A UNIX socket's mode is 0777 &^ umask, so before #687 that property
// was inherited from whatever umask the plugin runtime happened to
// set -- true by accident under the usual 0022, false under 0002.
// The test therefore installs a permissive umask of its own: under the
// runtime default it would pass against the old behaviour too, and
// prove nothing.
//
// Listen is driven for real rather than mirrored, so the guard cannot
// rot away from the code path it protects.
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

	// Wait for Listen to get as far as binding.
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

	// And the socket still works for the owner -- a guard that locked
	// the daemon out would also satisfy the assertion above.
	c, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("owner cannot connect to the restricted socket: %v", err)
	}
	_ = c.Close()
}
