// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// netnsDummy adds an up dummy in the test's own namespace and waits for the kernel's fe80 on it.
func netnsDummy(t *testing.T, name string) netlink.Link {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	l, err := netlink.LinkByName(name)
	if err == nil {
		err = netlink.LinkSetUp(l)
	}
	if err != nil {
		t.Fatalf("up %s: %v", name, err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		addrs, _ := util.DumpResult(netlink.AddrList(l, netlink.FAMILY_V6))
		if len(addrs) > 0 {
			break
		}
	}
	l, err = netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func readSysctl(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

// TestBridgeOwn_TheKernelShowsTheBridgeMadeAndRetired runs the create and the retire against a real kernel and reads
// the result back from it, not from the plugin (#903).
func TestBridgeOwn_TheKernelShowsTheBridgeMadeAndRetired(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	withStateDir(t, t.TempDir())
	parent := netnsDummy(t, "dh903p0")
	if item, err := parentAddressItem(parent); err != nil || item != "" {
		t.Fatalf("a fresh up dummy with its fe80 read as %q, %v; the rule must pass what the kernel adds itself", item, err)
	}
	opts := DHCPNetworkOptions{Mode: ModeBridge, Bridge: "dh903b0", Parent: "dh903p0"}
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	created, err := p.ensureBridge(context.Background(), opts, "create_network")
	if err != nil || !created {
		t.Fatalf("created %v, err %v", created, err)
	}
	br, err := netlink.LinkByName("dh903b0")
	if err != nil {
		t.Fatal(err)
	}
	parent, _ = netlink.LinkByName("dh903p0")
	switch {
	case br.Type() != "bridge" || br.Attrs().Alias != vlanOwnerAlias:
		t.Errorf("dh903b0 is %s with alias %q; want a marked bridge", br.Type(), br.Attrs().Alias)
	case br.Attrs().Flags&net.FlagUp == 0:
		t.Error("the bridge is down")
	case parent.Attrs().MasterIndex != br.Attrs().Index:
		t.Errorf("parent master %d, want %d", parent.Attrs().MasterIndex, br.Attrs().Index)
	case readSysctl(t, "/proc/sys/net/ipv6/conf/dh903b0/disable_ipv6") != "1":
		t.Error("the bridge has IPv6 on; the host would take an fe80 on the LAN through it")
	}
	if addrs, err := util.DumpResult(netlink.AddrList(br, netlink.FAMILY_ALL)); err != nil || len(addrs) != 0 {
		t.Errorf("the bridge carries %v (%v); the create gives the host no address on it", addrs, err)
	}

	p.retireBridge(context.Background(), "self", opts, "delete_network")
	if _, err := netlink.LinkByName("dh903b0"); !isLinkNotFound(err) {
		t.Errorf("the bridge outlived the retire: %v", err)
	}
	parent, _ = netlink.LinkByName("dh903p0")
	if parent.Attrs().MasterIndex != 0 || parent.Attrs().Promisc != 0 || parent.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("parent master %d, promiscuity %d, flags %v; want it released as found", parent.Attrs().MasterIndex,
			parent.Attrs().Promisc, parent.Attrs().Flags)
	}
}

// TestBridgeOwn_TheKernelRefusalsLeaveTheHostAsFound: an addressed parent, a routed parent, a port of another bridge
// and an unmarked bridge are each refused with the kernel state unchanged (#903).
func TestBridgeOwn_TheKernelRefusalsLeaveTheHostAsFound(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.docker = &fakeDocker{}

	addressed := netnsDummy(t, "dh903a0")
	if err := netlink.AddrAdd(addressed, &netlink.Addr{IPNet: mustCIDR(t, "192.0.2.0/24")}); err != nil {
		t.Fatal(err)
	}
	routed := netnsDummy(t, "dh903r0")
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: routed.Attrs().Index, Dst: mustCIDR(t, "198.51.100.0/24"), Scope: netlink.SCOPE_LINK}); err != nil {
		t.Fatal(err)
	}
	other := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "dh903o0"}}
	if err := netlink.LinkAdd(other); err != nil {
		t.Fatal(err)
	}
	other2, _ := netlink.LinkByName("dh903o0")
	port := netnsDummy(t, "dh903s0")
	if err := netlink.LinkSetMaster(port, other2); err != nil {
		t.Fatal(err)
	}
	netnsDummy(t, "dh903f0")

	for _, tc := range []struct {
		name, parent, bridge, text string
	}{
		{"an addressed parent", "dh903a0", "dh903b1", "carries the IPv4 address 192.0.2.0/24"},
		{"a routed parent", "dh903r0", "dh903b2", "carries the route 198.51.100.0/24"},
		{"a port of another bridge", "dh903s0", "dh903b3", "already a port of dh903o0"},
		{"an unmarked bridge", "dh903f0", "dh903o0", "this plugin did not make it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.ensureBridge(context.Background(), DHCPNetworkOptions{Mode: ModeBridge, Bridge: tc.bridge, Parent: tc.parent}, "create_network")
			if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v; want ErrIPAM containing %q", err, tc.text)
			}
			if tc.bridge != "dh903o0" {
				if _, err := netlink.LinkByName(tc.bridge); !isLinkNotFound(err) {
					t.Errorf("%s exists after the refusal: %v", tc.bridge, err)
				}
			}
		})
	}
	port, _ = netlink.LinkByName("dh903s0")
	free, _ := netlink.LinkByName("dh903f0")
	if port.Attrs().MasterIndex != other2.Attrs().Index || free.Attrs().MasterIndex != 0 {
		t.Errorf("port master %d, free master %d; want the refusals to move nothing", port.Attrs().MasterIndex, free.Attrs().MasterIndex)
	}
}

// TestBridgeOwn_TheKernelKeepsOneParentPerBridge: a second network naming the bridge with another NIC is refused with
// that NIC left alone, with ignore_conflicts or without, and the first network's delete removes the bridge (#903).
func TestBridgeOwn_TheKernelKeepsOneParentPerBridge(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	withStateDir(t, t.TempDir())
	stubFirewall(t, false, nil, nfDrop, nil)
	netnsDummy(t, "dh903m0")
	netnsDummy(t, "dh903m1")
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	create := func(id, parent, ignore string) error {
		return p.CreateNetwork(CreateNetworkRequest{
			NetworkID: id,
			Options: map[string]interface{}{util.OptionsKeyGeneric: map[string]interface{}{
				"bridge": "dh903b9", "parent": parent, "ignore_conflicts": ignore}},
			IPv4Data: []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
		})
	}
	if err := create(vlanNetA, "dh903m0", "false"); err != nil {
		t.Fatal(err)
	}
	for _, ignore := range []string{"false", "true"} {
		err := create(vlanNetB, "dh903m1", ignore)
		if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "takes one parent") {
			t.Errorf("ignore_conflicts=%s: err %v; want the one-parent refusal", ignore, err)
		}
		if l, err := netlink.LinkByName("dh903m1"); err != nil || l.Attrs().MasterIndex != 0 {
			t.Errorf("ignore_conflicts=%s: dh903m1 %v, %v; want it left without a master", ignore, l, err)
		}
	}
	if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: vlanNetA}); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName("dh903b9"); !isLinkNotFound(err) {
		t.Errorf("the bridge outlived its only network: %v", err)
	}
}

// TestNftForwardPolicy_AChainWithoutAPolicyIsUnread: a regular chain named FORWARD has no hook and so no policy; the
// read must not take it for ACCEPT (#903).
func TestNftForwardPolicy_AChainWithoutAPolicyIsUnread(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	nftBatch(t,
		nftMsg(unix.NFT_MSG_NEWTABLE, unix.NLM_F_CREATE, nl.NewRtAttr(unix.NFTA_TABLE_NAME, nl.ZeroTerminated("filter"))),
		nftMsg(unix.NFT_MSG_NEWCHAIN, unix.NLM_F_CREATE,
			nl.NewRtAttr(unix.NFTA_CHAIN_TABLE, nl.ZeroTerminated("filter")),
			nl.NewRtAttr(unix.NFTA_CHAIN_NAME, nl.ZeroTerminated("FORWARD"))))
	if got, err := nftForwardPolicy(); err == nil || !strings.Contains(err.Error(), "carries no policy") {
		t.Fatalf("policy %d, err %v; want the chain named as carrying no policy", got, err)
	}
	prevNF := bridgeNFCallIPTables
	t.Cleanup(func() { bridgeNFCallIPTables = prevNF })
	bridgeNFCallIPTables = func(string) (bool, error) { return true, nil }
	if why := firewallDropReason("lan0"); !strings.Contains(why, "cannot be read over nf_tables") {
		t.Errorf("reason %q; want the unread policy named", why)
	}
}

// nftBatch sends one nf_tables batch with msgs between BEGIN and END and waits for the acks (#903).
func nftBatch(t *testing.T, msgs ...*nl.NetlinkRequest) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		t.Fatalf("no netfilter netlink socket: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		t.Fatal(err)
	}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3})
	batch := func(typ int) []byte {
		req := nl.NewNetlinkRequest(typ, 0)
		b := req.Serialize()
		// res_id carries the subsystem, big-endian (include/uapi/linux/netfilter/nfnetlink.h).
		return append(b[:unix.SizeofNlMsghdr], 0, 0, 0, 0)
	}
	fixLen := func(b []byte) []byte {
		binary.NativeEndian.PutUint32(b, uint32(len(b)))
		return b
	}
	begin := fixLen(batch(unix.NFNL_MSG_BATCH_BEGIN))
	binary.BigEndian.PutUint16(begin[unix.SizeofNlMsghdr+2:], unix.NFNL_SUBSYS_NFTABLES)
	end := fixLen(batch(unix.NFNL_MSG_BATCH_END))
	binary.BigEndian.PutUint16(end[unix.SizeofNlMsghdr+2:], unix.NFNL_SUBSYS_NFTABLES)
	out := append([]byte{}, begin...)
	for _, m := range msgs {
		m.Flags |= unix.NLM_F_ACK
		out = append(out, m.Serialize()...)
	}
	out = append(out, end...)
	if err := unix.Sendto(fd, out, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<16)
	for acks := 0; acks < len(msgs); {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			t.Fatalf("nf_tables batch: %v", err)
		}
		replies, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range replies {
			if r.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if code := -int32(binary.NativeEndian.Uint32(r.Data)); code != 0 {
				t.Fatalf("nf_tables batch: %v", unix.Errno(code))
			}
			acks++
		}
	}
}

func nftMsg(typ int, flags int, attrs ...*nl.RtAttr) *nl.NetlinkRequest {
	req := nl.NewNetlinkRequest(unix.NFNL_SUBSYS_NFTABLES<<8|typ, flags)
	req.AddData(&nl.Nfgenmsg{NfgenFamily: unix.NFPROTO_IPV4, Version: nl.NFNETLINK_V0})
	for _, a := range attrs {
		req.AddData(a)
	}
	return req
}

func be32(v uint32) []byte {
	return binary.BigEndian.AppendUint32(nil, v)
}

// TestNftForwardPolicy_ReadsTheKernel reads the policy back from a chain the test makes, what `iptables-nft -P FORWARD
// DROP` leaves: table ip filter, base chain FORWARD on the forward hook (#903).
func TestNftForwardPolicy_ReadsTheKernel(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	if _, err := nftForwardPolicy(); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("err = %v in an empty namespace; want ENOENT, the error an iptables-legacy host gives", err)
	}
	prevNF := bridgeNFCallIPTables
	t.Cleanup(func() { bridgeNFCallIPTables = prevNF })
	bridgeNFCallIPTables = func(string) (bool, error) { return true, nil }
	if why := firewallDropReason("lan0"); !strings.Contains(why, "cannot be read over nf_tables") {
		t.Errorf("reason %q; want the unread policy named", why)
	}

	table := nl.NewRtAttr(unix.NFTA_CHAIN_TABLE, nl.ZeroTerminated("filter"))
	name := nl.NewRtAttr(unix.NFTA_CHAIN_NAME, nl.ZeroTerminated("FORWARD"))
	hook := nl.NewRtAttr(unix.NFTA_CHAIN_HOOK|unix.NLA_F_NESTED, nil)
	hook.AddRtAttr(unix.NFTA_HOOK_HOOKNUM, be32(unix.NF_INET_FORWARD))
	hook.AddRtAttr(unix.NFTA_HOOK_PRIORITY, be32(0))
	nftBatch(t,
		nftMsg(unix.NFT_MSG_NEWTABLE, unix.NLM_F_CREATE, nl.NewRtAttr(unix.NFTA_TABLE_NAME, nl.ZeroTerminated("filter"))),
		nftMsg(unix.NFT_MSG_NEWCHAIN, unix.NLM_F_CREATE, table, name, hook,
			nl.NewRtAttr(unix.NFTA_CHAIN_POLICY, be32(nfDrop)), nl.NewRtAttr(unix.NFTA_CHAIN_TYPE, nl.ZeroTerminated("filter"))))
	if got, err := nftForwardPolicy(); err != nil || got != nfDrop {
		t.Fatalf("policy %d, %v; want DROP (0)", got, err)
	}
	if why := firewallDropReason("lan0"); !strings.Contains(why, "is DROP") {
		t.Errorf("reason %q; want DROP named", why)
	}

	nftBatch(t, nftMsg(unix.NFT_MSG_NEWCHAIN, 0,
		nl.NewRtAttr(unix.NFTA_CHAIN_TABLE, nl.ZeroTerminated("filter")),
		nl.NewRtAttr(unix.NFTA_CHAIN_NAME, nl.ZeroTerminated("FORWARD")),
		nl.NewRtAttr(unix.NFTA_CHAIN_POLICY, be32(1))))
	if got, err := nftForwardPolicy(); err != nil || got != 1 {
		t.Fatalf("policy %d, %v; want ACCEPT (1)", got, err)
	}
	if why := firewallDropReason("lan0"); why != "" {
		t.Errorf("reason %q; want none under ACCEPT", why)
	}
}
