//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestConcurrency_DistinctLeases starts N containers on the same
// macvlan network in parallel and asserts each gets a distinct IP
// from the DHCP pool. Doubles as a deadlock smoke test:
// CreateEndpoint takes networkLock per-network, so a regression that
// upgraded that to a global lock or held it across the udhcpc -q call
// would serialize starts and quickly blow the timeout.
//
// N=4 keeps the test fast (one short-lease dnsmasq, one veth) while
// being enough to surface a serialization regression: 4 sequential
// 5-second udhcpc roundtrips would already exceed the per-container
// IPAcquisitionBudget if the lock were held wrong.
func TestConcurrency_DistinctLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const N = 4
	netName := "dh-itest-concurrency-net"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	type result struct {
		idx  int
		ipv4 string
		mac  string
		err  error
	}
	results := make(chan result, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results <- result{idx: i, err: fmt.Errorf("panic: %v", r)}
				}
			}()
			ctrName := fmt.Sprintf("dh-itest-concurrency-ctr-%d", i)
			_, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
			results <- result{idx: i, ipv4: ipv4, mac: mac}
		}(i)
	}
	wg.Wait()
	close(results)

	seenIPs := make(map[string]int, N)
	seenMACs := make(map[string]int, N)
	for r := range results {
		if r.err != nil {
			t.Errorf("worker %d: %v", r.idx, r.err)
			continue
		}
		harness.AssertIP(t, r.ipv4)
		if other, dup := seenIPs[r.ipv4]; dup {
			t.Errorf("duplicate IP %s assigned to workers %d and %d", r.ipv4, other, r.idx)
		}
		if other, dup := seenMACs[r.mac]; dup {
			t.Errorf("duplicate MAC %s assigned to workers %d and %d", r.mac, other, r.idx)
		}
		seenIPs[r.ipv4] = r.idx
		seenMACs[r.mac] = r.idx
		t.Logf("worker %d: ip=%s mac=%s", r.idx, r.ipv4, r.mac)
	}
	if len(seenIPs) != N {
		t.Fatalf("expected %d distinct IPs, got %d", N, len(seenIPs))
	}
}
