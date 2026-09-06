package runtime_test

import (
	"context"
	"log"
	"os"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/runtime"
)

// ExampleClient is the smallest real use of this library, and it is compiled
// rather than quoted: the README's Usage section is this function, byte for
// byte, so a reader who copies it gets code the suite builds.
//
// The interface name comes from the environment because a lease needs a link
// with a server on it. Unset — which is how every run of the suite sees it —
// the example returns before it opens anything, which is why its expected
// output is empty. A compile-only example, with no expected output at all,
// would be DECLARED and never LISTED, and the arbiter refuses that.
func ExampleClient() {
	iface := os.Getenv("DHCP_GOLIB_EXAMPLE_IFACE")
	if iface == "" {
		return
	}

	client, err := runtime.NewClient(runtime.ClientConfig{
		Interface: iface,
		Params:    proto.DefaultParams(nil),
	})
	if err != nil {
		log.Print(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := client.Run(ctx); err != nil {
			log.Print(err)
		}
	}()

	for ev := range client.Events() {
		if ev.Kind != lease.Acquired {
			continue
		}
		log.Printf("%s via %s until %s", ev.Lease.Addr, ev.Lease.Gateway, ev.Lease.Expire)
		break
	}

	client.Release()
	// Output:
}
