// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const (
	renameTestIndex   = 7
	renameTestLocated = "dh-abcdef"
	renameTestRenamed = "eth0"
)

// v6RenameKernel stands in for the sandbox: a sysctl tree under dir and
// the link table the resolvers read; rename moves both, as conf/<if>
// follows the device in the kernel (#1065).
type v6RenameKernel struct {
	t         *testing.T
	dir       string
	links     map[string]int
	inSandbox bool
	byIndex   int
	onByIndex func(call int)
	failIndex bool
}

func newV6RenameKernel(t *testing.T, links map[string]int) *v6RenameKernel {
	t.Helper()
	k := &v6RenameKernel{t: t, dir: t.TempDir(), links: links}
	for name := range links {
		k.seed(name, "1")
	}

	prevEnter, prevByIndex, prevByName := v6EnterSandbox, v6LinkNameByIndex, v6LinkIndexByName
	v6EnterSandbox = func(_ *dhcpManager, work func(string)) error {
		k.inSandbox = true
		defer func() { k.inSandbox = false }()
		work(k.dir)
		return nil
	}
	v6LinkNameByIndex = func(index int) (string, error) {
		if !k.inSandbox {
			t.Error("the link name was resolved outside the sandbox namespace")
		}
		k.byIndex++
		if k.failIndex {
			return "", errors.New("Link not found")
		}
		var found string
		for name, idx := range k.links {
			if idx == index {
				found = name
			}
		}
		if k.onByIndex != nil {
			k.onByIndex(k.byIndex)
		}
		if found == "" {
			return "", errors.New("Link not found")
		}
		return found, nil
	}
	v6LinkIndexByName = func(name string) (int, error) {
		if idx, ok := k.links[name]; ok {
			return idx, nil
		}
		return 0, errors.New("Link not found")
	}
	stubKernelRouteTable(t, nil, nil, nil)
	t.Cleanup(func() {
		v6EnterSandbox, v6LinkNameByIndex, v6LinkIndexByName = prevEnter, prevByIndex, prevByName
	})
	return k
}

// seed writes disable_ipv6 and every guard knob at a value the guard
// has to move (#1065).
func (k *v6RenameKernel) seed(name, disable string) {
	k.t.Helper()
	if err := os.MkdirAll(filepath.Join(k.dir, name), 0o755); err != nil {
		k.t.Fatalf("mkdir: %v", err)
	}
	write := func(knob, v string) {
		if err := os.WriteFile(filepath.Join(k.dir, name, knob), []byte(v+"\n"), 0o644); err != nil {
			k.t.Fatalf("seed %s/%s: %v", name, knob, err)
		}
	}
	write("disable_ipv6", disable)
	for knob, want := range dhcp.RouterAdvertGuardContract() {
		write(knob, notTheContractValue(want))
	}
}

func (k *v6RenameKernel) rename(from, to string) {
	k.t.Helper()
	if err := os.Rename(filepath.Join(k.dir, from), filepath.Join(k.dir, to)); err != nil {
		k.t.Fatalf("rename %s -> %s: %v", from, to, err)
	}
	k.links[to] = k.links[from]
	delete(k.links, from)
}

// assertPrepared checks disable_ipv6 and the guard's knobs on name.
func (k *v6RenameKernel) assertPrepared(name string) {
	k.t.Helper()
	if got := v6LinkKnob(k.t, k.dir, name, "disable_ipv6"); got != "0" {
		k.t.Errorf("%s: disable_ipv6 reads %q, want 0", name, got)
	}
	contract := dhcp.RouterAdvertGuardContract()
	if len(contract) != 3 {
		k.t.Fatalf("the guard contract has %d knobs, want 3", len(contract))
	}
	for knob, want := range contract {
		if got := v6LinkKnob(k.t, k.dir, name, knob); got != want {
			k.t.Errorf("%s: %s reads %q, want %q", name, knob, got, want)
		}
	}
}

func renameTestManager(p *Plugin) *dhcpManager {
	m := &dhcpManager{ctrLink: &netlink.Device{LinkAttrs: netlink.LinkAttrs{
		Name: renameTestLocated, Index: renameTestIndex,
	}}}
	return m.withPlugin(p)
}

func assertV6Counters(t *testing.T, p *Plugin, enable, guard int32) {
	t.Helper()
	if got := p.ipv6LinkEnableFailures.Load(); got != enable {
		t.Errorf("ipv6_link_enable_failures = %d, want %d", got, enable)
	}
	if got := p.routerAdvertGuardFailures.Load(); got != guard {
		t.Errorf("router_advert_guard_failures = %d, want %d", got, guard)
	}
}

func TestEnsureIPv6Enabled_LinkRenamedBetweenLocateAndPrepare(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestRenamed: renameTestIndex})
	p := &Plugin{}
	renameTestManager(p).ensureIPv6Enabled()
	k.assertPrepared(renameTestRenamed)
	assertV6Counters(t, p, 0, 0)
}

func TestEnsureIPv6Enabled_LinkRenamedBetweenResolveAndWrite(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	k.onByIndex = func(call int) {
		if call == 1 {
			k.rename(renameTestLocated, renameTestRenamed)
		}
	}
	p := &Plugin{}
	renameTestManager(p).ensureIPv6Enabled()
	k.assertPrepared(renameTestRenamed)
	assertV6Counters(t, p, 0, 0)
	if k.byIndex != 2 {
		t.Errorf("the index was resolved %d times, want 2: one resolve, one retry", k.byIndex)
	}
}

func TestPrepareIPv6Link_LinkRenamedAfterTheClearBeforeTheGuard(t *testing.T) {
	// The state a rename after the disable_ipv6 write leaves: the old
	// name answers the clear, the knobs only under the new one (#1065).
	k := newV6RenameKernel(t, map[string]int{renameTestRenamed: renameTestIndex})
	if err := os.MkdirAll(filepath.Join(k.dir, renameTestLocated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(k.dir, renameTestLocated, "disable_ipv6"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where a knob was fails the write and the read, as a
	// vanished procfs entry does, root or not (#1065).
	for knob := range dhcp.RouterAdvertGuardContract() {
		if err := os.Mkdir(filepath.Join(k.dir, renameTestLocated, knob), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	k.seed(renameTestRenamed, "0")
	calls := 0
	v6LinkNameByIndex = func(int) (string, error) {
		calls++
		if calls == 1 {
			return renameTestLocated, nil
		}
		return renameTestRenamed, nil
	}
	changed, guard, err := renameTestManager(&Plugin{}).prepareIPv6Link()
	if err != nil || guard.Failures != 0 {
		t.Fatalf("prepareIPv6Link: err %v, %d guard failure(s): %v", err, guard.Failures, guard.Err)
	}
	if !changed {
		t.Error("disable_ipv6 was written under the old name and the call reports nothing changed")
	}
	k.assertPrepared(renameTestRenamed)
}

func TestPrepareIPv6Link_NameThatNeverSettlesEndsInOneError(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{})
	var last string
	v6LinkNameByIndex = func(int) (string, error) {
		k.byIndex++
		if k.byIndex <= 10*v6LinkAttempts {
			last = "dh-" + strings.Repeat("x", k.byIndex)
		}
		return last, nil
	}
	p := &Plugin{}
	_, _, err := renameTestManager(p).prepareIPv6Link()
	if err == nil {
		t.Fatal("prepareIPv6Link succeeded on a link with no sysctl directory under any name")
	}
	if k.byIndex != v6LinkAttempts {
		t.Errorf("the index was resolved %d times, want %d", k.byIndex, v6LinkAttempts)
	}
	if !strings.Contains(err.Error(), filepath.Join(k.dir, last)+"/") {
		t.Errorf("error %q does not name the last name tried, %q", err, last)
	}

	k.byIndex = 0
	renameTestManager(p).ensureIPv6Enabled()
	assertV6Counters(t, p, 1, 0)
}

func TestPrepareIPv6Link_LinkGoneIsOneAttempt(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{})
	k.failIndex = true
	p := &Plugin{}
	_, _, err := renameTestManager(p).prepareIPv6Link()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want the write's own not-exist error", err)
	}
	if k.byIndex != 2 {
		t.Errorf("the index was resolved %d times, want 2: no retry for a link that is gone", k.byIndex)
	}
	renameTestManager(p).ensureIPv6Enabled()
	assertV6Counters(t, p, 1, 0)
}

func TestPrepareIPv6Link_UnresolvableIndexKeepsTheLocatedName(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	k.failIndex = true
	p := &Plugin{}
	renameTestManager(p).ensureIPv6Enabled()
	k.assertPrepared(renameTestLocated)
	assertV6Counters(t, p, 0, 0)
}

func TestPrepareIPv6Link_FailureOnAStableNameIsNotRetried(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestRenamed: renameTestIndex})
	p := filepath.Join(k.dir, renameTestRenamed, "disable_ipv6")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	plugin := &Plugin{}
	renameTestManager(plugin).ensureIPv6Enabled()
	if k.byIndex != 2 {
		t.Errorf("the index was resolved %d times, want 2: a failure on a name that did not move is final", k.byIndex)
	}
	assertV6Counters(t, plugin, 1, 0)
}

func TestPrepareIPv6Link_NameTakenByAnotherLinkIsRetriedOnOurs(t *testing.T) {
	const other = 8
	k := newV6RenameKernel(t, map[string]int{"eth1": renameTestIndex, renameTestRenamed: other})
	k.onByIndex = func(call int) {
		if call == 1 {
			k.links = map[string]int{renameTestRenamed: renameTestIndex, "eth1": other}
		}
	}
	p := &Plugin{}
	renameTestManager(p).ensureIPv6Enabled()
	k.assertPrepared(renameTestRenamed)
	assertV6Counters(t, p, 0, 0)
}

func TestPrepareIPv6Link_LinkGoneAfterAnotherTookItsName(t *testing.T) {
	const other = 8
	setup := func(t *testing.T) {
		k := newV6RenameKernel(t, map[string]int{"eth1": renameTestIndex})
		k.onByIndex = func(call int) {
			if call == 1 {
				k.links = map[string]int{"eth1": other}
			}
		}
	}

	t.Run("the kernel's reason is returned", func(t *testing.T) {
		setup(t)
		_, _, err := renameTestManager(&Plugin{}).prepareIPv6Link()
		if err == nil || !strings.Contains(err.Error(), "Link not found") {
			t.Fatalf("err = %v, want the resolver's link-not-found", err)
		}
	})
	t.Run("counted once", func(t *testing.T) {
		setup(t)
		p := &Plugin{}
		renameTestManager(p).ensureIPv6Enabled()
		assertV6Counters(t, p, 1, 0)
	})
}

func TestPrepareIPv6Link_NameTakenOnEveryPassEndsAtTheCap(t *testing.T) {
	setup := func(t *testing.T) *int {
		k := newV6RenameKernel(t, map[string]int{})
		v6LinkNameByIndex = func(int) (string, error) {
			k.byIndex++
			name := "eth" + strings.Repeat("x", k.byIndex)
			k.seed(name, "1")
			return name, nil
		}
		passes := 0
		v6LinkIndexByName = func(string) (int, error) {
			passes++
			if passes > 10*v6LinkAttempts {
				return renameTestIndex, nil
			}
			return renameTestIndex + passes, nil
		}
		return &passes
	}

	t.Run("unstable error at the cap", func(t *testing.T) {
		passes := setup(t)
		_, _, err := renameTestManager(&Plugin{}).prepareIPv6Link()
		if !errors.Is(err, errV6LinkNameUnstable) {
			t.Fatalf("err = %v, want errV6LinkNameUnstable", err)
		}
		if *passes != v6LinkAttempts {
			t.Errorf("%d passes, want %d", *passes, v6LinkAttempts)
		}
	})
	t.Run("counted once", func(t *testing.T) {
		setup(t)
		p := &Plugin{}
		renameTestManager(p).ensureIPv6Enabled()
		assertV6Counters(t, p, 1, 0)
	})
}
