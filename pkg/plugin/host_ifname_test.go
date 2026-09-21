// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The name rule, at its boundaries (#978).
//
// The rule is the operator's, not the plugin's: `docs/reference.md`
// states it in words and an operator reads `ip link` expecting what it
// says. So the cases below are the sentences of that paragraph, one
// test case each, and the expected names are written out rather than
// computed -- a table that derived its answers the way the subject does
// would agree with any rule at all.
func TestDeriveHostIfname_TheRuleAtItsBoundaries(t *testing.T) {
	// 64 hex, the shape libnetwork actually sends.
	const ep = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

	for _, tc := range []struct {
		name   string
		source string
		ep     string
		want   string
		why    string
	}{
		{"a plain name is itself", "web", ep, "web",
			"a name the kernel would take is not rewritten"},
		{"fourteen bytes are untouched", "abcdefghijklmn", ep, "abcdefghijklmn",
			"one under the limit"},
		{"fifteen bytes are untouched", "abcdefghijklmno", ep, "abcdefghijklmno",
			"IFNAMSIZ-1 exactly, which the kernel takes"},
		{"sixteen bytes are truncated", "abcdefghijklmnop", ep, "abcdefghi-a1b2c",
			"one over the limit is the first case the endpoint suffix appears in"},
		{"a compose name keeps its project prefix", "myproj-web-1", ep, "myproj-web-1",
			"the common case is under the limit and survives whole"},
		{"a long compose name ends in the endpoint", "myproject-frontend-1", ep, "myproject-a1b2c",
			"the suffix is the same five hex the dh- name carries, so it points back"},
		{"illegal bytes become dashes", "web@host:1", ep, "web-host-1",
			"the substitution keeps the name the same length, so truncation stays predictable"},
		{"a leading illegal byte is dropped", "_web", ep, "web",
			"a kernel-legal name starts with a letter or a digit"},
		{"leading illegal bytes are all dropped", "---.web", ep, "web",
			"the drop repeats until the first byte is alphanumeric"},
		{"a name with nothing legal in it is refused", "///", ep, "",
			"every byte becomes a dash and every dash is then dropped"},
		{"an empty name is refused", "", ep, "",
			"a container with no name to take"},
		{"non-ASCII becomes dashes and may vanish", "äöü", ep, "",
			"UTF-8 bytes are each outside the rule, so nothing alphanumeric is left to start with"},
		{"non-ASCII beside something legal keeps the legal part", "webä", ep, "web--",
			"only LEADING bytes are dropped, so the trailing dashes stay and are legal"},
		{"a short endpoint ID still truncates", "abcdefghijklmnop", "abc", "abcdefghijk-abc",
			"defensive: vethPairNames tolerates a short ID and so does this"},
		{"an empty endpoint ID truncates without a suffix", "abcdefghijklmnop", "", "abcdefghijklmno",
			"there is nothing to disambiguate with, and a plain cut is still a legal name"},
		{"an endpoint ID outside the name alphabet is refused", "abcdefghijklmnop", "ab/cd", "",
			"the suffix is copied from the ID and not rewritten, so the last rule is what stands " +
				"between a malformed ID and a name the kernel refuses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveHostIfname(tc.source, tc.ep)
			if got != tc.want {
				t.Errorf("deriveHostIfname(%q, %q) = %q, want %q (%s)", tc.source, tc.ep, got, tc.want, tc.why)
			}
			if got != "" && !dhcp.ValidIfaceName(got) {
				t.Errorf("deriveHostIfname(%q) produced %q, which dhcp.ValidIfaceName refuses: the kernel "+
					"would refuse the rename and the operator would read a counter instead of a name",
					tc.source, got)
			}
			if len(got) > hostIfnameMaxLen {
				t.Errorf("deriveHostIfname(%q) produced %d bytes, and the kernel takes %d: MEASURED, a "+
					"16-byte name is refused with ERANGE", tc.source, len(got), hostIfnameMaxLen)
			}
		})
	}
}

// Two long names sharing their first nine bytes must not become one
// name. This is the whole reason the suffix is keyed on the endpoint.
func TestDeriveHostIfname_TwoLongNamesOnOneHostStayDistinct(t *testing.T) {
	const epA = "aaaaaaaaaaaa1111"
	const epB = "bbbbbbbbbbbb2222"
	a := deriveHostIfname("myproject-frontend-1", epA)
	b := deriveHostIfname("myproject-frontend-2", epB)
	if a == b {
		t.Errorf("two containers whose names differ after byte 9 both derived %q. The second rename "+
			"would fail on a collision its operator cannot see in either name", a)
	}
	if !strings.HasSuffix(a, "-aaaaa") || !strings.HasSuffix(b, "-bbbbb") {
		t.Errorf("truncated names %q and %q do not end in their own endpoint IDs' first five hex, so "+
			"nothing correlates them back to docker network inspect", a, b)
	}
}

// A hostname is not unique on a host. The suffix must not be derived
// from the source string, or two containers sharing one hostname derive
// one name.
func TestDeriveHostIfname_OneHostnameOnTwoEndpointsDerivesTwoNames(t *testing.T) {
	a := deriveHostIfname("shared-hostname-value", "1111111111111111")
	b := deriveHostIfname("shared-hostname-value", "2222222222222222")
	if a == b {
		t.Errorf("both endpoints derived %q from the same hostname. Two containers may share a "+
			"--hostname, and the second would lose its rename to a collision", a)
	}
}

func TestHostIfnameSource_PicksTheFieldTheOptionNames(t *testing.T) {
	for _, tc := range []struct {
		opt  string
		want string
	}{
		{HostIfnameOff, ""},
		{HostIfnameContainerName, "web"},
		{HostIfnameHostname, "web-host"},
	} {
		opts := DHCPNetworkOptions{HostIfname: tc.opt}
		// The Docker API's Name field carries a leading slash, which no
		// interface name may contain and ValidIfaceName refuses.
		if got := opts.hostIfnameSource("/web", "web-host"); got != tc.want {
			t.Errorf("host_ifname=%q sourced %q, want %q", tc.opt, got, tc.want)
		}
	}
}

func TestParseHostIfname_ATypoFailsTheCreate(t *testing.T) {
	for _, v := range []string{HostIfnameOff, HostIfnameContainerName, HostIfnameHostname} {
		got, err := parseHostIfname(v)
		if err != nil || got != v {
			t.Errorf("parseHostIfname(%q) = %q, %v; want %q, nil", v, got, err, v)
		}
	}
	_, err := parseHostIfname("containername")
	if err == nil {
		t.Fatal("parseHostIfname accepted `containername`: a near miss that selected the default " +
			"silently is a network whose operator believes its links are named and that names none")
	}
	if !errors.Is(err, util.ErrIPAM) {
		t.Errorf("parseHostIfname refusal is %v, want an ErrIPAM: Docker turns that into a create "+
			"failure the operator sees", err)
	}
}

func TestValidateModeOptions_HostIfnameIsRefusedWhereNothingStaysOnTheHost(t *testing.T) {
	for _, mode := range []string{ModeMacvlan, ModeIPvlan} {
		err := validateModeOptions(DHCPNetworkOptions{
			Mode: mode, Parent: "eth0", HostIfname: HostIfnameContainerName,
		})
		if !errors.Is(err, util.ErrModeMismatch) {
			t.Errorf("mode=%s with host_ifname set: err = %v, want ErrModeMismatch. The child link is "+
				"moved into the container and leaves nothing on the host, so accepting the option "+
				"means an operator reads ip link and finds the generated names still there", mode, err)
		}
	}
	if err := validateModeOptions(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0"}); err != nil {
		t.Errorf("mode=macvlan without host_ifname: err = %v, want nil. The refusal must be about the "+
			"option and not about the mode", err)
	}
	if err := validateModeOptions(DHCPNetworkOptions{
		Mode: ModeBridge, Bridge: "br0", HostIfname: HostIfnameContainerName,
	}); err != nil {
		t.Errorf("mode=bridge with host_ifname=container_name: err = %v, want nil", err)
	}
	if err := validateModeOptions(DHCPNetworkOptions{
		Mode: ModeBridge, Bridge: "br0", HostIfname: "nonsense",
	}); !errors.Is(err, util.ErrIPAM) {
		t.Errorf("mode=bridge with an unknown host_ifname: err = %v, want ErrIPAM", err)
	}
}

// renameLog records what the two netlink seams were asked to do, in
// order, so an arm is judged on what reached the kernel rather than on
// which counter moved.
type renameLog struct {
	lookups  []string
	names    []string
	altNames []string

	lookupErr error
	nameErr   error
	altErr    error
	nameErrs  map[string]error
}

func withRenameSeams(t *testing.T, r *renameLog) {
	t.Helper()
	prevBy, prevSet, prevAlt := nlLinkByName, nlLinkSetName, nlLinkAddAltName
	nlLinkByName = func(name string) (netlink.Link, error) {
		r.lookups = append(r.lookups, name)
		if r.lookupErr != nil {
			return nil, r.lookupErr
		}
		return &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 7}}, nil
	}
	nlLinkSetName = func(_ netlink.Link, name string) error {
		r.names = append(r.names, name)
		if err, ok := r.nameErrs[name]; ok {
			return err
		}
		return r.nameErr
	}
	nlLinkAddAltName = func(_ netlink.Link, name string) error {
		r.altNames = append(r.altNames, name)
		return r.altErr
	}
	t.Cleanup(func() {
		nlLinkByName, nlLinkSetName, nlLinkAddAltName = prevBy, prevSet, prevAlt
	})
}

// aBridgeEndpoint is a manager on a bridge network that asked for named
// host links. The endpoint ID is 64 hex, so vethPairNames' 12-hex
// prefix and deriveHostIfname's 5-hex suffix are both real.
func aBridgeEndpoint(t *testing.T, opt string) (*dhcpManager, *Plugin) {
	t.Helper()
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{
		NetworkID:  "net-1",
		EndpointID: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
	}, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", HostIfname: opt}).withPlugin(p)
	return m, p
}

func TestRenameHostLink_TheLinkTakesTheContainersNameAndKeepsTheOldOne(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if len(r.names) != 1 || r.names[0] != "web" {
		t.Fatalf("the kernel was asked to set names %v, want exactly [web]. The counter is intent; this "+
			"is what an operator's ip link would print", r.names)
	}
	if len(r.altNames) != 1 || r.altNames[0] != "dh-a1b2c3d4e5f6" {
		t.Errorf("the old name was kept on the link as %v, want exactly [dh-a1b2c3d4e5f6]. Four sites "+
			"re-derive that name and look it up without reading one back, and DeleteEndpoint treats a "+
			"miss as the normal end of a teardown, so a rename without the altname leaves the veth on "+
			"the bridge for the life of the host", r.altNames)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1: it is the domain the two failure counters are read "+
			"against, and a zero here makes their zeros mean nothing", got)
	}
	if got := p.hostIfnameConflicts.Load() + p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("a refusal was counted (%d) on a rename the kernel took", got)
	}
}

func TestRenameHostLink_TheHostnameSourceIsTheOtherValue(t *testing.T) {
	m, _ := aBridgeEndpoint(t, HostIfnameHostname)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if len(r.names) != 1 || r.names[0] != "web-host" {
		t.Errorf("host_ifname=hostname set names %v, want exactly [web-host]: the two values name two "+
			"different fields of the same inspect", r.names)
	}
}

func TestRenameHostLink_AnInspectThatNeverAnsweredRenamesNothing(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(false, "", "")

	if len(r.lookups)+len(r.names)+len(r.altNames) != 0 {
		t.Errorf("netlink was asked %v / %v / %v for an attach whose inspect never answered. The only "+
			"name available then is one nothing decided", r.lookups, r.names, r.altNames)
	}
	if got := p.hostIfnamesApplied.Load() + p.hostIfnameConflicts.Load() + p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("%d counters of this change moved on an attach whose name never arrived. "+
			"hostname_lookup_failures has already said why, and a second counter for one event makes "+
			"both of them ambiguous", got)
	}
}

func TestRenameHostLink_AnUnsetOptionAndAParentAttachedModeAskTheKernelNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"the option is off", DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0"}},
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", HostIfname: HostIfnameContainerName}},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0", HostIfname: HostIfnameContainerName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}, tc.opts).withPlugin(p)
			r := &renameLog{}
			withRenameSeams(t, r)

			m.renameHostLink(true, "/web", "web-host")

			if len(r.lookups)+len(r.names) != 0 {
				t.Errorf("netlink was asked %v / %v. CreateNetwork refuses this combination, so reaching "+
					"it means stored options were hand-edited, and renaming a link on a mode that has "+
					"none is worse than doing nothing", r.lookups, r.names)
			}
			if got := p.hostIfnamesApplied.Load(); got != 0 {
				t.Errorf("host_ifnames_applied = %d on a mode with no host-side link: the denominator "+
					"would report work nothing did", got)
			}
		})
	}
}

func TestRenameHostLink_ANameAlreadyOnThisHostIsItsOwnCounter(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{nameErr: unix.EEXIST}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if got := p.hostIfnameConflicts.Load(); got != 1 {
		t.Errorf("host_ifname_conflicts = %d, want 1. Interface names are one namespace shared with "+
			"every network and every NIC on the box, and this is the only refusal whose remedy is to "+
			"rename something", got)
	}
	if got := p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("host_ifname_failures = %d, want 0: folding a taken name into the general refusal "+
			"leaves an operator with no way to tell it from a kernel that would not rename", got)
	}
	if len(r.altNames) != 0 {
		t.Errorf("the link was given altnames %v after a rename that did not happen", r.altNames)
	}
	if got := p.hostIfnamesApplied.Load(); got != 0 {
		t.Errorf("host_ifnames_applied = %d, want 0", got)
	}
}

func TestRenameHostLink_AnyOtherRefusalKeepsTheGeneratedName(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{nameErr: unix.EBUSY}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if got := p.hostIfnameFailures.Load(); got != 1 {
		t.Errorf("host_ifname_failures = %d, want 1. A kernel that refuses to rename a running link "+
			"answers EBUSY, and the endpoint keeps its lease and its generated name either way", got)
	}
	if got := p.hostIfnameConflicts.Load(); got != 0 {
		t.Errorf("host_ifname_conflicts = %d, want 0: nothing was taken", got)
	}
}

func TestRenameHostLink_AnAltnameThatCannotBeAddedUndoesTheRename(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{altErr: errors.New("no IFLA_PROP_LIST support")}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if len(r.names) != 2 || r.names[0] != "web" || r.names[1] != "dh-a1b2c3d4e5f6" {
		t.Fatalf("the kernel was asked to set names %v, want [web dh-a1b2c3d4e5f6]. Without the old "+
			"name on the link, nothing that looks the link up can find it: DeleteEndpoint reads a "+
			"missing link as a finished teardown and returns nil, so the veth stays on the bridge", r.names)
	}
	if got := p.hostIfnameFailures.Load(); got != 1 {
		t.Errorf("host_ifname_failures = %d, want 1", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 0 {
		t.Errorf("host_ifnames_applied = %d, want 0: the link ended up with the name it started with", got)
	}
}

// The undo can fail too, and then the link really is unreachable by the
// name every other site derives. It is counted once and said loudly;
// what it must not do is report success.
func TestRenameHostLink_AnUndoThatFailsIsStillNotAnApplication(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{
		altErr:   errors.New("no IFLA_PROP_LIST support"),
		nameErrs: map[string]error{"dh-a1b2c3d4e5f6": unix.EEXIST},
	}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if got := p.hostIfnamesApplied.Load(); got != 0 {
		t.Errorf("host_ifnames_applied = %d, want 0", got)
	}
	if got := p.hostIfnameFailures.Load(); got != 1 {
		t.Errorf("host_ifname_failures = %d, want 1", got)
	}
	if got := p.hostIfnameConflicts.Load(); got != 0 {
		t.Errorf("host_ifname_conflicts = %d, want 0: the EEXIST here is the UNDO failing, not the "+
			"container's name being taken, and charging it to the conflict counter would send an "+
			"operator to rename a container that has nothing wrong with it", got)
	}
}

func TestRenameHostLink_ANameWithNothingLegalInItAsksTheKernelNothing(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameHostname)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "///")

	if len(r.lookups)+len(r.names) != 0 {
		t.Errorf("netlink was asked %v / %v for a name the rule refuses. The refusal is the plugin's "+
			"and belongs before the syscall, or the kernel's error is the only account of it", r.lookups, r.names)
	}
	if got := p.hostIfnameFailures.Load(); got != 1 {
		t.Errorf("host_ifname_failures = %d, want 1: a container whose name derives to nothing keeps "+
			"its generated link name, and silence there is a question an operator cannot answer", got)
	}
}

func TestRenameHostLink_AHostLinkThatIsNotThereIsCountedAndNotRenamed(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{lookupErr: netlink.LinkNotFoundError{}}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web-host")

	if len(r.names) != 0 {
		t.Errorf("a rename was attempted on a link that was not found: %v", r.names)
	}
	if got := p.hostIfnameFailures.Load(); got != 1 {
		t.Errorf("host_ifname_failures = %d, want 1", got)
	}
}

// The one case where the container's name is already the generated one.
// It must not go to the kernel: the altname add that follows a rename
// would then ask for a name the link already has, which is EEXIST, and
// the undo would rename it to itself.
func TestRenameHostLink_ANameThatIsAlreadyTheGeneratedOneAsksTheKernelNothing(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/dh-a1b2c3d4e5f6", "web-host")

	if len(r.lookups)+len(r.names)+len(r.altNames) != 0 {
		t.Errorf("netlink was asked %v / %v / %v to give the link the name it already has",
			r.lookups, r.names, r.altNames)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1: the link carries the name the operator asked for, "+
			"which is what the counter is about", got)
	}
}

// What `docker network inspect --verbose` prints for a renamed link.
//
// An operator who set host_ifname reads that field and `ip link`, and
// the two must not disagree: the lookup resolves through the altname,
// so publishing the name it was looked up BY would print a name the
// kernel no longer shows anywhere.
func TestEndpointOperInfo_PublishesTheNameTheLinkHas(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()

	const netID = "0123456789abcdef0123456789abcdef"
	const epID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	opts := DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test", HostIfname: HostIfnameContainerName}
	if err := saveNetwork(netID, opts, nil); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}

	prev := nlLinkByName
	nlLinkByName = func(name string) (netlink.Link, error) {
		if name != "dh-a1b2c3d4e5f6" {
			t.Errorf("the link was looked up as %q, want the generated name: that name is kept on the "+
				"renamed link as an altname and is the only handle this plugin writes down", name)
		}
		return &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "web", Index: 7}}, nil
	}
	t.Cleanup(func() { nlLinkByName = prev })

	res, err := p.EndpointOperInfo(context.Background(), InfoRequest{NetworkID: netID, EndpointID: epID})
	if err != nil {
		t.Fatalf("EndpointOperInfo: %v", err)
	}
	if got := res.Value["veth_host"]; got != "web" {
		t.Errorf("veth_host = %q, want %q. That is the field an operator reads to find the interface "+
			"in ip link, and the generated name is not what ip link prints for it", got, "web")
	}
}

// The wiring between #961's lookup and #978's rename, which neither
// step's own drives can see: that both run, in that order, on one
// attach, and that the rename runs on BOTH routes into it.
//
// The order is load-bearing. The lookup fills the hostname field and the
// rename reads it, so a rename moved above the lookup derives its name
// from an empty string and silently leaves every endpoint on a
// `hostname` network with its generated name.
func TestAfterAttach_TheNameReachesTheClientAndThenTheLink(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameHostname)
	client := &fakeJoinClient{}
	m.setHealthClient(client)
	r := &renameLog{}
	withRenameSeams(t, r)

	ctrHostname := ""
	ctrName := "/web"
	m.afterAttach(newJoinPhases(), false, func() error { ctrHostname = "web1"; return nil },
		&ctrName, &ctrHostname)

	if got := client.names; len(got) != 1 || got[0] != "web1" {
		t.Errorf("the running client was told %v, want exactly [web1] (#961)", got)
	}
	if got := r.names; len(got) != 1 || got[0] != "web1" {
		t.Fatalf("the kernel was asked to set names %v, want exactly [web1]. The rename reads the field "+
			"the lookup fills, so a rename that does not run, or runs first, leaves the host-side link "+
			"named dh-a1b2c3d4e5f6 with nothing saying so", got)
	}
	if got := r.altNames; len(got) != 1 || got[0] != "dh-a1b2c3d4e5f6" {
		t.Errorf("the old name was kept as %v, want exactly [dh-a1b2c3d4e5f6]", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 1 {
		t.Errorf("hostnames_applied_late = %d, want 1: the same attach did both", got)
	}
}

// The route the guard's position exists for: register_dns and the PID
// fallback have the name before the client starts and never enter the
// lookup at all. A rename nested inside that lookup's `if` would rename
// nothing here, on those hosts only, with no counter to say so.
func TestAfterAttach_ARouteThatAlreadyHadTheNameStillRenamesTheLink(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameHostname)
	r := &renameLog{}
	withRenameSeams(t, r)

	ctrName, ctrHostname := "/web", "web1"
	m.afterAttach(newJoinPhases(), true, func() error {
		t.Error("the daemon was asked again on a route that already had the name: the attach pays a " +
			"second inspect while the daemon is inside ContainerStart (#406)")
		return nil
	}, &ctrName, &ctrHostname)

	if got := r.names; len(got) != 1 || got[0] != "web1" {
		t.Fatalf("the kernel was asked to set names %v, want exactly [web1]", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1", got)
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: nothing was looked up here", got)
	}
}

// A daemon that never answered renames nothing. The link keeps the name
// it was created with and #961's hostname_lookup_failures has already
// said why, so #978 adds no second counter for the same event.
func TestAfterAttach_ADaemonThatNeverAnsweredRenamesNothing(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	r := &renameLog{}
	withRenameSeams(t, r)

	ctrName, ctrHostname := "", ""
	m.afterAttach(newJoinPhases(), false, func() error {
		return errors.New("daemon is inside ContainerStart")
	}, &ctrName, &ctrHostname)

	if len(r.lookups) != 0 || len(r.names) != 0 {
		t.Fatalf("netlink was asked %v and told to set %v after a lookup that never answered: the "+
			"container's name is not known, so the only name available is one nothing decided",
			r.lookups, r.names)
	}
	if got := p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("host_ifname_failures = %d, want 0: hostname_lookup_failures (%d) already counts this "+
			"event, and counting it twice makes one daemon stall read as two faults",
			got, p.hostnameLookupFailures.Load())
	}
	if got := p.hostnameLookupFailures.Load(); got != 1 {
		t.Errorf("hostname_lookup_failures = %d, want 1", got)
	}
}

// fakeKernel models what the kernel does to a link's NAMES, which the
// seam fixture above deliberately does not: it answers lookups on the
// altname as well as the primary name, and it refuses a name it is
// already holding under either.
//
// MEASURED on 6.12.107 with the module this repo pins, under
// `unshare -Urn`, on a veth that is up and enslaved to a bridge:
//   - LinkSetName to the name the link already has returns 0
//   - LinkAddAltName with an altname the link already has is EEXIST
//   - LinkSetName to a name the link holds as an ALTNAME is EEXIST,
//     because altnames share the kernel's name hash
//   - LinkByName on an altname resolves and returns the link under its
//     primary name
type fakeKernel struct {
	name     string
	altNames map[string]bool
	// taken is every name some OTHER link on this host holds, as a
	// primary name or an altname; the kernel keeps one table for both,
	// which is why a collision can come from either.
	taken map[string]bool
	sets  []string
	adds  []string
}

func (k *fakeKernel) has(name string) bool {
	return name == k.name || k.altNames[name] || k.taken[name]
}

func withKernelSeams(t *testing.T, k *fakeKernel) {
	t.Helper()
	if k.altNames == nil {
		k.altNames = map[string]bool{}
	}
	prevBy, prevSet, prevAlt := nlLinkByName, nlLinkSetName, nlLinkAddAltName
	nlLinkByName = func(name string) (netlink.Link, error) {
		if k.taken[name] {
			// Some other link holds it, so this one is not what comes
			// back; nothing here looks a foreign link up, so refusing
			// is the honest answer.
			return nil, unix.ENODEV
		}
		if !k.has(name) {
			return nil, unix.ENODEV
		}
		return &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: k.name, Index: 7}}, nil
	}
	nlLinkSetName = func(_ netlink.Link, name string) error {
		k.sets = append(k.sets, name)
		if name == k.name {
			return nil
		}
		if k.has(name) {
			return unix.EEXIST
		}
		k.name = name
		return nil
	}
	nlLinkAddAltName = func(_ netlink.Link, name string) error {
		k.adds = append(k.adds, name)
		if k.has(name) {
			return unix.EEXIST
		}
		k.altNames[name] = true
		return nil
	}
	t.Cleanup(func() {
		nlLinkByName, nlLinkSetName, nlLinkAddAltName = prevBy, prevSet, prevAlt
	})
}

// A second attach over an already renamed link is the ORDINARY event,
// not a pathology: recovery calls Start again for every endpoint it
// rebuilds after a plugin restart, and its synthesised request carries
// no sandbox key, so the inspect runs first and the rename runs with
// the daemon's answer in hand.
//
// Without the read-back at host_ifname.go the second pass walks the
// whole path over a healthy link: the rename to the name it already has
// succeeds, the altname is EEXIST, the undo is EEXIST, and the operator
// gets a warn counter and the Error line that says the link must be
// removed by hand. About a link that is on the bridge, correctly named,
// and found by teardown without trouble.
func TestRenameHostLink_ASecondPassOverTheSameLinkIsNotAFailure(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	k := &fakeKernel{name: "dh-a1b2c3d4e5f6"}
	withKernelSeams(t, k)

	m.renameHostLink(true, "/web", "web-host")

	if k.name != "web" || !k.altNames["dh-a1b2c3d4e5f6"] {
		t.Fatalf("after the first attach the link is %q with altnames %v, want web keeping "+
			"dh-a1b2c3d4e5f6", k.name, k.altNames)
	}

	m.renameHostLink(true, "/web", "web-host")

	if k.name != "web" || !k.altNames["dh-a1b2c3d4e5f6"] {
		t.Errorf("after the second attach the link is %q with altnames %v, want it untouched",
			k.name, k.altNames)
	}
	if len(k.sets) != 1 || len(k.adds) != 1 {
		t.Errorf("the kernel was told to set names %v and add altnames %v across two attaches, want "+
			"exactly one of each: a link that already carries both names has nothing left to do and "+
			"every call from here answers EEXIST", k.sets, k.adds)
	}
	if got := p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("host_ifname_failures = %d, want 0. The link is named, on its bridge and found by "+
			"teardown, so this reads as a fault on every running container every time the plugin "+
			"restarts, and it puts the health document in warn", got)
	}
	if got := p.hostIfnameConflicts.Load(); got != 0 {
		t.Errorf("host_ifname_conflicts = %d, want 0: the name it collided with is its own", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 2 {
		t.Errorf("host_ifnames_applied = %d, want 2: both attaches left the link carrying the name its "+
			"network asked for, and that counter is the domain the two above are read against", got)
	}
}

// The fixture's own control. If the fake stopped resolving altnames or
// stopped refusing a name the link already holds, the drive above would
// pass for the wrong reason, and so would every mutant it is meant to
// kill.
func TestFakeKernel_AnswersAsTheKernelWasMeasuredTo(t *testing.T) {
	k := &fakeKernel{name: "dh-a1b2c3d4e5f6"}
	withKernelSeams(t, k)

	if _, err := nlLinkByName("nothing-here"); !errors.Is(err, unix.ENODEV) {
		t.Errorf("a name no link has resolved with err = %v, want ENODEV", err)
	}
	link, err := nlLinkByName("dh-a1b2c3d4e5f6")
	if err != nil {
		t.Fatalf("the link's own name did not resolve: %v", err)
	}
	if err := nlLinkSetName(link, "web"); err != nil {
		t.Fatalf("rename to a free name: %v", err)
	}
	if err := nlLinkAddAltName(link, "dh-a1b2c3d4e5f6"); err != nil {
		t.Fatalf("adding the old name as an altname: %v", err)
	}
	if _, err := nlLinkByName("dh-a1b2c3d4e5f6"); err != nil {
		t.Errorf("the altname did not resolve: %v. Every lookup of the generated name in this plugin "+
			"goes through it, so a fixture that cannot do this cannot judge the rename at all", err)
	}
	if err := nlLinkSetName(link, "web"); err != nil {
		t.Errorf("rename to the name the link already has: err = %v, want nil", err)
	}
	if err := nlLinkAddAltName(link, "dh-a1b2c3d4e5f6"); !errors.Is(err, unix.EEXIST) {
		t.Errorf("adding an altname the link already has: err = %v, want EEXIST", err)
	}
	if err := nlLinkSetName(link, "dh-a1b2c3d4e5f6"); !errors.Is(err, unix.EEXIST) {
		t.Errorf("renaming to a name the link holds as an ALTNAME: err = %v, want EEXIST. Altnames "+
			"share the kernel's name hash, which is why the undo cannot be assumed to work", err)
	}

	// A name some OTHER link on this host holds. One table for names
	// and altnames, so the refusal is the same either way, and a lookup
	// of it does not return this link.
	k.taken = map[string]bool{"api": true}
	if err := nlLinkSetName(link, "api"); !errors.Is(err, unix.EEXIST) {
		t.Errorf("renaming onto a name another interface holds: err = %v, want EEXIST", err)
	}
	if _, err := nlLinkByName("api"); !errors.Is(err, unix.ENODEV) {
		t.Errorf("looking up a foreign link's name returned this link: err = %v, want ENODEV. A fake "+
			"that answered with this link would make a collision look like a link already named", err)
	}

	// The library does not rewrite the struct on a successful rename,
	// MEASURED by reading netlink v1.3.1 link_linux.go LinkSetName: it
	// builds the request from base and returns the kernel's answer. The
	// fake matches that, so a drive here cannot pass because a fake
	// updated a field the real one leaves alone.
	if got := link.Attrs().Name; got != "dh-a1b2c3d4e5f6" {
		t.Errorf("the link struct's name is %q after renames through the seam, want it untouched at "+
			"%q, the name it carried when the lookup returned it: the library leaves the struct "+
			"alone and a fake that did not would hide where the altname question may be asked",
			got, "dh-a1b2c3d4e5f6")
	}
}

// A hostname this plugin refused to put on the wire does not get to name
// an interface instead. safeHostname drops a --hostname carrying a
// control character because the container chose that value; the
// derivation would have turned the same value into a legal name, so the
// refusal is repeated where the name is chosen.
func TestRenameHostLink_AHostnameTheWireRefusedDoesNotNameTheLink(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameHostname)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web1\nduid 00:03:00:01:be:ef:be:ef:be:ef")

	if len(r.names) != 0 {
		t.Errorf("the kernel was asked to set names %v from a hostname this plugin refuses to send as "+
			"DHCP option 12. The derivation would have made it legal, which is exactly why the "+
			"refusal has to be repeated here", r.names)
	}
	if got := p.hostIfnameFailures.Load(); got != 1 {
		t.Errorf("host_ifname_failures = %d, want 1: the link keeps its generated name and the "+
			"operator is told why", got)
	}
	if got := p.unsafeHostnamesRejected.Load(); got != 0 {
		t.Errorf("unsafe_hostnames_rejected = %d, want 0 from this path: the counter belongs to the "+
			"one call that decides what goes on the wire, and counting it here would report one "+
			"container as two rejections", got)
	}
}

// The other direction: a hostname the wire accepts still names the link.
func TestRenameHostLink_AHostnameTheWireAcceptsStillNamesTheLink(t *testing.T) {
	m, _ := aBridgeEndpoint(t, HostIfnameHostname)
	r := &renameLog{}
	withRenameSeams(t, r)

	m.renameHostLink(true, "/web", "web1")

	if len(r.names) != 1 || r.names[0] != "web1" {
		t.Errorf("the kernel was asked to set names %v, want exactly [web1]", r.names)
	}
}

// The same second pass, with the wanted name MOVED instead of standing
// still: `docker rename web api` on a running container, then a plugin
// restart. Recovery inspects, the daemon says /api, and the link is
// named web with the generated name on it as an altname.
//
// The read-back does not fire here, because the link is not yet named
// what this attach wants. What the path must not do is offer the kernel
// an altname the link already has: that is EEXIST, and the undo that
// answers it is EEXIST too, so a link that ends up named api, keeping
// its altname and sitting on its bridge, is reported as one that has to
// be removed by hand.
func TestRenameHostLink_ARenamedContainerTakesItsNewNameWithoutAFailure(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	k := &fakeKernel{name: "dh-a1b2c3d4e5f6"}
	withKernelSeams(t, k)

	m.renameHostLink(true, "/web", "web-host")
	if k.name != "web" {
		t.Fatalf("the first attach left the link named %q, want web", k.name)
	}

	m.renameHostLink(true, "/api", "web-host")

	if k.name != "api" {
		t.Errorf("after the container was renamed the link is %q, want api. The name is re-derived on "+
			"every attach and nothing about it is persisted", k.name)
	}
	if !k.altNames["dh-a1b2c3d4e5f6"] {
		t.Errorf("the link no longer carries dh-a1b2c3d4e5f6 as an altname (altnames %v): every lookup "+
			"this plugin makes of the generated name goes through it", k.altNames)
	}
	if len(k.adds) != 1 {
		t.Errorf("the kernel was told to add altnames %v across two attaches, want exactly one: the "+
			"second pass resolved the link THROUGH that altname, so offering it again is EEXIST and "+
			"the undo that answers it is EEXIST as well", k.adds)
	}
	if got := p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("host_ifname_failures = %d, want 0. The link is named api, keeps its altname and is "+
			"on its bridge, so this is the same wrong alarm as the recovery pass, raised by a name "+
			"that moved instead of one that stayed", got)
	}
	if got := p.hostIfnameConflicts.Load(); got != 0 {
		t.Errorf("host_ifname_conflicts = %d, want 0", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 2 {
		t.Errorf("host_ifnames_applied = %d, want 2: both attaches left the link carrying the name its "+
			"network asked for at the time", got)
	}
}

// The other direction, so the skip above cannot widen into "never add
// the altname": a first attach, where the link still answers to the
// generated name as its own, must add it.
func TestRenameHostLink_AFirstAttachStillPutsTheOldNameBackOnTheLink(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	k := &fakeKernel{name: "dh-a1b2c3d4e5f6"}
	withKernelSeams(t, k)

	m.renameHostLink(true, "/web", "web-host")

	if len(k.adds) != 1 || k.adds[0] != "dh-a1b2c3d4e5f6" {
		t.Fatalf("the kernel was told to add altnames %v, want exactly [dh-a1b2c3d4e5f6]. Without it "+
			"the rename is a link nothing can find, and DeleteEndpoint reads a miss as a finished "+
			"teardown", k.adds)
	}
	if !k.altNames["dh-a1b2c3d4e5f6"] {
		t.Errorf("the link does not carry the generated name as an altname (altnames %v)", k.altNames)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1", got)
	}
}

// A renamed container whose new name is already taken by something else
// on the host. This is the collision arm reached on a SECOND pass, where
// the link already carries both names and has a name worth keeping.
//
// The link must keep what it has -- its current name and its altname --
// and the conflict counter, not the failure counter, must say so: an
// operator reading host_ifname_conflicts is told to rename one of the
// two things, and one reading host_ifname_failures is told the plugin
// or the kernel misbehaved.
func TestRenameHostLink_ARenamedContainerWhoseNewNameIsTakenKeepsWhatItHas(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	k := &fakeKernel{name: "dh-a1b2c3d4e5f6"}
	withKernelSeams(t, k)

	m.renameHostLink(true, "/web", "web-host")
	if k.name != "web" || !k.altNames["dh-a1b2c3d4e5f6"] {
		t.Fatalf("the first attach left the link %q with altnames %v, want web keeping "+
			"dh-a1b2c3d4e5f6", k.name, k.altNames)
	}
	k.taken = map[string]bool{"api": true}

	m.renameHostLink(true, "/api", "web-host")

	if k.name != "web" {
		t.Errorf("the link is named %q after a refused rename, want it to keep web", k.name)
	}
	if !k.altNames["dh-a1b2c3d4e5f6"] {
		t.Errorf("the link lost its altname over a refused rename (altnames %v): the rename never "+
			"happened, so nothing about the link's names may have changed", k.altNames)
	}
	if got := p.hostIfnameConflicts.Load(); got != 1 {
		t.Errorf("host_ifname_conflicts = %d, want 1. The name the container asked for belongs to "+
			"another interface on this host, and that is the counter whose docs row tells the "+
			"operator to rename one of the two", got)
	}
	if got := p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("host_ifname_failures = %d, want 0: nothing here is the plugin's fault or the "+
			"kernel's, and the two counters carry different instructions", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1: the first attach applied a name and this one did "+
			"not", got)
	}
}

// THE ROUTE THAT FILLS THE NAME AFTER THE ATTACH, which is every
// attach on an engine that lets this plugin enter the sandbox through
// its netns key.
//
// MEASURED, engine 29.8.0 against 29.8.1 on the same code: the key
// route replaced the container PID route (sandbox_key_entries +1 where
// sandbox_pid_fallbacks was +1), and the PID route was the one that
// inspected the container on the way in. Without it the daemon is not
// asked until the lookup below, so the container's name does not exist
// when the attach reaches this call, and a caller that hands over the
// value it holds at that moment hands over an empty string. What the
// operator saw: the container's name on the wire, from the same
// inspect, and the host-side link still called dh-a1b2c3d4e5f6, with
// host_ifname_failures counting a name the daemon had answered
// perfectly well.
//
// The hostname has always been passed as a pointer, which is why a
// `hostname` network never showed this and no test had the shape to
// catch it. Both fields come from the one lookup and both are read
// after it runs.
func TestAfterAttach_TheContainerNameReachesTheLinkWhenTheLookupIsLate(t *testing.T) {
	m, p := aBridgeEndpoint(t, HostIfnameContainerName)
	client := &fakeJoinClient{}
	m.setHealthClient(client)
	r := &renameLog{}
	withRenameSeams(t, r)

	ctrName, ctrHostname := "", ""
	m.afterAttach(newJoinPhases(), false, func() error {
		ctrName, ctrHostname = "/web1", "web1"
		return nil
	}, &ctrName, &ctrHostname)

	if got := r.names; len(got) != 1 || got[0] != "web1" {
		t.Fatalf("the kernel was asked to set names %v, want exactly [web1]. The lookup answered with "+
			"the container's name and the link kept dh-a1b2c3d4e5f6, which is the whole of #978 not "+
			"happening on this route", got)
	}
	if got := r.altNames; len(got) != 1 || got[0] != "dh-a1b2c3d4e5f6" {
		t.Errorf("the old name was kept as %v, want exactly [dh-a1b2c3d4e5f6]", got)
	}
	if got := p.hostIfnamesApplied.Load(); got != 1 {
		t.Errorf("host_ifnames_applied = %d, want 1", got)
	}
	if got := p.hostIfnameFailures.Load(); got != 0 {
		t.Errorf("host_ifname_failures = %d, want 0: the daemon answered with a name an interface may "+
			"carry, and counting that as a failure points an operator at the container's name", got)
	}
}
