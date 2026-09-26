// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const setHostnameNetnsEnv = "DHCP_SETHOSTNAME_NETNS_CHILD"

// TestSetHostname_ReachesTheStartedLibraryClient starts real library clients on a dummy link in a fresh network
// namespace, so the name is shown reaching the client Start built, for each family (#961, #1029).
func TestSetHostname_ReachesTheStartedLibraryClient(t *testing.T) {
	const name = "TestSetHostname_ReachesTheStartedLibraryClient"
	if os.Getenv(setHostnameNetnsEnv) == "" {
		cmd := exec.Command(os.Args[0], "-test.run", "^"+name+"$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), setHostnameNetnsEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		if os.Getuid() != 0 {
			cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
			cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
			cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		}
		out, err := cmd.CombinedOutput()
		text := string(out)
		switch {
		case err != nil && (errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EACCES)):
			t.Skipf("this host cannot create the namespace: %v", err)
		case strings.Contains(text, "--- SKIP: "):
			t.Skipf("the namespaced run skipped:\n%s", text)
		case err != nil || !strings.Contains(text, "--- PASS: "+name+" "):
			t.Fatalf("the namespaced run failed (%v):\n%s", err, text)
		}
		return
	}

	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "sh0"}}); err != nil {
		t.Skipf("no CAP_NET_ADMIN in this namespace (an AppArmor userns restriction?): %v", err)
	}
	link, err := netlink.LinkByName("sh0")
	if err == nil {
		err = netlink.LinkSetUp(link)
	}
	if err != nil {
		t.Fatalf("bring sh0 up: %v", err)
	}
	mac := testMAC(t)
	ns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer func() { _ = ns.Close() }()

	for _, tc := range []struct {
		family string
		opts   DHCPClientOptions
	}{
		{"v4", DHCPClientOptions{MAC: mac}},
		{"v6 with register_dns", DHCPClientOptions{MAC: mac, V6: true, NetNS: &ns, HonorRouterAdverts: true, FQDN: "web1",
			Identity6: Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xc0, 0xa8, 0x63, 0x07}, IAID: 0xc0a86307}}},
	} {
		t.Run(tc.family, func(t *testing.T) {
			c, err := NewDHCPClient("sh0", &tc.opts)
			if err != nil {
				t.Fatalf("NewDHCPClient: %v", err)
			}
			if _, err := c.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = c.Finish(ctx)
			}()
			if err := c.SetHostname("web1"); err != nil {
				t.Fatalf("SetHostname on a started %s client = %v, want nil", tc.family, err)
			}
			// The library takes the name on its own loop, so it is read back until it lands or two seconds pass.
			got := ""
			for deadline := time.Now().Add(2 * time.Second); got != "web1" && time.Now().Before(deadline); {
				if c.client6 != nil {
					got = c.client6.Hostname()
				} else if c.client != nil {
					got = c.client.Hostname()
				}
				time.Sleep(20 * time.Millisecond)
			}
			if got != "web1" {
				t.Errorf("the %s library client's name is %q after SetHostname, want web1", tc.family, got)
			}
		})
	}
}
