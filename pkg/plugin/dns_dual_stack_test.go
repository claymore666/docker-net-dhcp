// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

type writtenResolvConf struct {
	mu     sync.Mutex
	writes []string
}

func captureResolvConf(t *testing.T) *writtenResolvConf {
	t.Helper()
	w := &writtenResolvConf{}
	prev := resolvConfWriter
	resolvConfWriter = func(_ int, _ string, dns, searchList []string, searchDomain, iface string) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.writes = append(w.writes, string(buildResolvConf(dns, searchList, searchDomain, iface)))
		return nil
	}
	t.Cleanup(func() { resolvConfWriter = prev })
	return w
}

func (w *writtenResolvConf) last(t *testing.T) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.writes) == 0 {
		t.Fatal("propagateDNS wrote no resolv.conf")
	}
	return w.writes[len(w.writes)-1]
}

func nameservers(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

func searchLine(content string) string {
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "search ") {
			return l
		}
	}
	return ""
}

func dualStackDNSManager(t *testing.T) *dhcpManager {
	t.Helper()
	m, _ := dnsPropagationManager(selfCgroupLeaf(t, os.Getpid()))
	return m
}

// Order is v4 servers then v6 servers, each in the order its family supplied, duplicates dropped on first
// occurrence; the search line lists the v4 domains then the v6 ones, deduplicated the same way (#1250).
func TestPropagateDNS_DualStackKeepsBothFamilies(t *testing.T) {
	w := captureResolvConf(t)
	m := dualStackDNSManager(t)

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.53"}, Domain: "lan.example"})
	m.propagateDNS(true, dhcp.Info{DNSServers: []string{"2001:db8::53"}})

	got := w.last(t)
	if want := []string{"192.0.2.53", "2001:db8::53"}; strings.Join(nameservers(got), ",") != strings.Join(want, ",") {
		t.Errorf("nameservers after a v4 then a v6 write = %v, want %v\n%s", nameservers(got), want, got)
	}
	if want := "search lan.example"; searchLine(got) != want {
		t.Errorf("search line after a v4 then a v6 write = %q, want %q (the v6 write carried no search list)\n%s", searchLine(got), want, got)
	}

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.53"}, Domain: "lan.example"})
	got = w.last(t)
	if want := []string{"192.0.2.53", "2001:db8::53"}; strings.Join(nameservers(got), ",") != strings.Join(want, ",") {
		t.Errorf("nameservers after a further v4 renew = %v, want %v\n%s", nameservers(got), want, got)
	}
}

func TestPropagateDNS_DualStackMergesSearchListsAndDropsDuplicates(t *testing.T) {
	w := captureResolvConf(t)
	m := dualStackDNSManager(t)

	m.propagateDNS(true, dhcp.Info{DNSServers: []string{"2001:db8::53", "192.0.2.53"}, SearchList: []string{"ra.example", "lan.example"}})
	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.53"}, SearchList: []string{"lan.example", "corp.example"}})

	got := w.last(t)
	if want := "192.0.2.53,2001:db8::53"; strings.Join(nameservers(got), ",") != want {
		t.Errorf("nameservers = %v, want %s (v4 first, a server both families gave once)\n%s", nameservers(got), want, got)
	}
	if want := "search lan.example corp.example ra.example"; searchLine(got) != want {
		t.Errorf("search line = %q, want %q (v4 domains first, duplicates dropped)\n%s", searchLine(got), want, got)
	}
}

func TestPropagateDNS_DualStackFamilyReplacesOnlyItself(t *testing.T) {
	w := captureResolvConf(t)
	m := dualStackDNSManager(t)

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.53"}})
	m.propagateDNS(true, dhcp.Info{DNSServers: []string{"2001:db8::53"}})
	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.54"}})

	got := w.last(t)
	if want := "192.0.2.54,2001:db8::53"; strings.Join(nameservers(got), ",") != want {
		t.Errorf("nameservers = %v, want %s: the renewed v4 set replaces the old v4 set, not the v6 one\n%s", nameservers(got), want, got)
	}
}

// RFC 8106 section 6.1: a family that goes silent keeps its resolvers, and an empty list writes nothing.
func TestPropagateDNS_DualStackEmptyListWritesNothing(t *testing.T) {
	w := captureResolvConf(t)
	m := dualStackDNSManager(t)

	m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.53"}})
	n := len(w.writes)
	m.propagateDNS(true, dhcp.Info{})
	if len(w.writes) != n {
		t.Errorf("an empty v6 list wrote %d new resolv.conf, want none", len(w.writes)-n)
	}
	m.propagateDNS(true, dhcp.Info{DNSServers: []string{"2001:db8::53"}})
	if want := "192.0.2.53,2001:db8::53"; strings.Join(nameservers(w.last(t)), ",") != want {
		t.Errorf("nameservers = %v, want %s", nameservers(w.last(t)), want)
	}
}

// Both families' goroutines call propagateDNS at once. A write that carries one family and lands after the other
// family's write would erase it, so the writer is slowed for exactly that shape, and the last file must hold both (#1250).
func TestPropagateDNS_ConcurrentFamiliesLastWriteHoldsBoth(t *testing.T) {
	var (
		mu     sync.Mutex
		last   string
		writer = func(_ int, _ string, dns, searchList []string, searchDomain, iface string) error {
			content := string(buildResolvConf(dns, searchList, searchDomain, iface))
			if len(nameservers(content)) < 2 {
				time.Sleep(3 * time.Millisecond)
			}
			mu.Lock()
			last = content
			mu.Unlock()
			return nil
		}
	)
	prev := resolvConfWriter
	resolvConfWriter = writer
	t.Cleanup(func() { resolvConfWriter = prev })

	for i := 0; i < 100; i++ {
		m := dualStackDNSManager(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			m.propagateDNS(false, dhcp.Info{DNSServers: []string{"192.0.2.53"}, Domain: "lan.example"})
		}()
		go func() {
			defer wg.Done()
			<-start
			m.propagateDNS(true, dhcp.Info{DNSServers: []string{"2001:db8::53"}})
		}()
		close(start)
		wg.Wait()

		mu.Lock()
		got := last
		mu.Unlock()
		if want := "192.0.2.53,2001:db8::53"; strings.Join(nameservers(got), ",") != want {
			t.Fatalf("run %d: last resolv.conf nameservers = %v, want %s\n%s", i, nameservers(got), want, got)
		}
		if want := "search lan.example"; searchLine(got) != want {
			t.Fatalf("run %d: last resolv.conf search line = %q, want %q\n%s", i, searchLine(got), want, got)
		}
	}
}
