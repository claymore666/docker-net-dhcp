// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"path"
	"strings"
	"testing"
)

const kea6TestSubnet = "fd00:6470:6865::/64"

type parsedKea6 struct {
	Dhcp6 struct {
		Interfaces struct {
			Interfaces []string `json:"interfaces"`
		} `json:"interfaces-config"`
		Lease struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Persist bool   `json:"persist"`
		} `json:"lease-database"`
		Valid     int `json:"valid-lifetime"`
		Renew     int `json:"renew-timer"`
		Rebind    int `json:"rebind-timer"`
		Preferred int `json:"preferred-lifetime"`
		Subnets   []struct {
			Subnet    string `json:"subnet"`
			Interface string `json:"interface"`
			Pools     []struct {
				Pool string `json:"pool"`
			} `json:"pools"`
			PDPools []struct {
				Prefix       string `json:"prefix"`
				PrefixLen    int    `json:"prefix-len"`
				DelegatedLen int    `json:"delegated-len"`
			} `json:"pd-pools"`
		} `json:"subnet6"`
		Loggers []struct {
			Name          string           `json:"name"`
			OutputOptions []map[string]any `json:"output_options"`
		} `json:"loggers"`
	}
}

func parseKea6(t *testing.T, c Kea6Config) parsedKea6 {
	t.Helper()
	var p parsedKea6
	if err := json.Unmarshal([]byte(c.JSON()), &p); err != nil {
		t.Fatalf("the rendered configuration is not JSON: %v\n%s", err, c.JSON())
	}
	return p
}

func baseKea6() Kea6Config {
	return Kea6Config{Interface: "dh-itest-k6s", Subnet: kea6TestSubnet, PoolStart: Kea6PoolStart, PoolEnd: Kea6PoolEnd}
}

func TestKea6Config_DefaultsAreTheLibrarysAndThereIsNoPDPool(t *testing.T) {
	p := parseKea6(t, baseKea6()).Dhcp6
	if p.Valid != 600 || p.Preferred != 400 || p.Renew != 300 || p.Rebind != 480 {
		t.Errorf("valid/preferred/T1/T2 = %d/%d/%d/%d, want 600/400/300/480", p.Valid, p.Preferred, p.Renew, p.Rebind)
	}
	if len(p.Subnets) != 1 || len(p.Subnets[0].PDPools) != 0 {
		t.Fatalf("want one subnet and no pd-pools unless asked, got %+v", p.Subnets)
	}
	s := p.Subnets[0]
	if s.Subnet != kea6TestSubnet || s.Interface != "dh-itest-k6s" || len(s.Pools) != 1 ||
		s.Pools[0].Pool != Kea6PoolStart+" - "+Kea6PoolEnd {
		t.Errorf("subnet = %+v", s)
	}
	if got := p.Interfaces.Interfaces; len(got) != 1 || got[0] != "dh-itest-k6s" {
		t.Errorf("interfaces = %v, want only the server link", got)
	}
	if p.Lease.Type != "memfile" || !p.Lease.Persist {
		t.Errorf("lease database = %+v, want a persistent memfile", p.Lease)
	}
	if len(p.Loggers) != 1 || p.Loggers[0].Name != "kea-dhcp6" || len(p.Loggers[0].OutputOptions) != 1 {
		t.Errorf("loggers = %+v: the key must be output_options, which Kea 2.4 and 2.6 both take", p.Loggers)
	}
}

func TestKea6Config_CallerTimersAndPDPoolAreRendered(t *testing.T) {
	c := baseKea6()
	WithKea6PD()(&c)
	WithKea6Timers(40, 70)(&c)
	WithKea6Lifetimes(120, 90)(&c)
	p := parseKea6(t, c).Dhcp6
	if p.Renew != 40 || p.Rebind != 70 || p.Valid != 120 || p.Preferred != 90 {
		t.Errorf("T1/T2/valid/preferred = %d/%d/%d/%d, want 40/70/120/90", p.Renew, p.Rebind, p.Valid, p.Preferred)
	}
	pd := p.Subnets[0].PDPools
	if len(pd) != 1 || pd[0].PrefixLen != 48 || pd[0].DelegatedLen != 64 {
		t.Fatalf("pd-pools = %+v, want one /48 delegated as /64", pd)
	}
	pool := netip.PrefixFrom(netip.MustParseAddr(pd[0].Prefix), pd[0].PrefixLen)
	if !netip.MustParsePrefix("fd00::/8").Contains(pool.Addr()) {
		t.Errorf("the delegation pool %s is not in unique-local space", pool)
	}
}

func TestKea6Config_AnUnsetTimerIsTheDefaultNotZero(t *testing.T) {
	c := baseKea6()
	WithKea6Timers(0, 0)(&c)
	p := parseKea6(t, c).Dhcp6
	if p.Renew != Kea6RenewSec || p.Rebind != Kea6RebindSec {
		t.Errorf("T1/T2 = %d/%d, want the defaults %d/%d: a zero timer lets Kea pick half the lifetime", p.Renew, p.Rebind,
			Kea6RenewSec, Kea6RebindSec)
	}
}

func TestKea6StatePathsAreOnThePackagedProfilesAllowList(t *testing.T) {
	conf := path.Join(Kea6ConfDir, Kea6ConfFile)
	lease := path.Join(Kea6LeaseDir, Kea6LeaseFile)
	p := parseKea6(t, baseKea6())
	paths := []string{
		conf, lease, path.Join(Kea6LeaseDir, Kea6ServerIDFile), lease + ".completed",
		path.Join(Kea6PidDir, Kea6PidFile), path.Join(Kea6LockDir, Kea6LockFile), path.Join(Kea6LogDir, Kea6LogFile),
		p.Dhcp6.Lease.Name,
	}
	for _, l := range p.Dhcp6.Loggers {
		for _, o := range l.OutputOptions {
			out, _ := o["output"].(string)
			if out == "stdout" || out == "stderr" || out == "syslog" {
				t.Errorf("logger output %q: a stream is an fd the profile re-checks on an enforcing host, and readiness reads "+
					"the log file", out)
			}
			paths = append(paths, out)
		}
	}
	for _, q := range paths {
		if !Kea6PathAllowed(q) {
			t.Errorf("%s is outside what the Ubuntu kea-dhcp6 profile allows: it would pass on the Debian pool and be denied "+
				"on an enforcing host", q)
		}
	}
	if path.Base(conf) != "kea-dhcp6.conf" {
		t.Errorf("the config is named %q: the profile's PID-file rule is written for kea-dhcp6.conf", path.Base(conf))
	}
	for _, q := range []string{"/tmp/kea-leases6.csv", "/var/lib/kea/kea-leases4.csv", "/var/lib/kea/other", "/var/lib/keax/kea-leases6.csv",
		"/etc/keax/a", "/run/kea/other.pid", "/run/lock/kea/other", "/tmp/dh-itest-kea6-1/kea-dhcp6.log", "/var/log/kea/kea-dhcp4.log"} {
		if Kea6PathAllowed(q) {
			t.Errorf("Kea6PathAllowed(%q) = true, want false", q)
		}
	}
}

const kea24Header = "address,duid,valid_lifetime,expire,subnet_id,pref_lifetime,lease_type,iaid,prefix_len,fqdn_fwd,fqdn_rev,hostname,hwaddr,state,user_context,hwtype,hwaddr_source\n"
const kea26Header = "address,duid,valid_lifetime,expire,subnet_id,pref_lifetime,lease_type,iaid,prefix_len,fqdn_fwd,fqdn_rev,hostname,hwaddr,state,user_context,hwtype,hwaddr_source,pool_id\n"

const keaDUID = "00:03:00:01:02:42:ac:11:00:02"

func TestParseKea6Leases_ReadsByNameAcrossBothHeaders(t *testing.T) {
	for name, header := range map[string]string{"2.4": kea24Header, "2.6": kea26Header} {
		t.Run(name, func(t *testing.T) {
			data := header +
				"fd00:6470:6865::1000," + keaDUID + ",600,2000000600,1,400,0,11,128,0,0,,,0,,,,\n" +
				"fd00:98:0:1::,00:03:00:01:AA:BB:CC:DD:EE:FF,600,2000000601,1,400,2,12,64,0,0,,,0,,,,\n"
			rows := ParseKea6Leases(data)
			if len(rows) != 2 {
				t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
			}
			na, pd := rows[0], rows[1]
			if na.Addr != "fd00:6470:6865::1000" || na.DUID != keaDUID || na.LeaseType != Kea6LeaseNA || na.PrefixLen != 128 ||
				na.Valid != 600 || na.Expire != 2000000600 {
				t.Errorf("IA_NA row = %+v", na)
			}
			if pd.LeaseType != Kea6LeasePD || pd.PrefixLen != 64 || pd.DUID != "00:03:00:01:aa:bb:cc:dd:ee:ff" {
				t.Errorf("IA_PD row = %+v: lease_type 2 is a delegated prefix, DUIDs are lower-cased", pd)
			}
		})
	}
}

func TestKea6LeaseTypeConstantsAreKeas(t *testing.T) {
	if Kea6LeaseNA != 0 || Kea6LeaseTA != 1 || Kea6LeasePD != 2 {
		t.Errorf("NA/TA/PD = %d/%d/%d, Kea writes 0/1/2: type 1 is a temporary address, not a prefix", Kea6LeaseNA, Kea6LeaseTA, Kea6LeasePD)
	}
}

func TestKea6Held(t *testing.T) {
	const now = 1000
	row := func(addr, duid string, valid, expire int64, typ, plen, state int) Kea6Row {
		return Kea6Row{Addr: addr, DUID: duid, Valid: valid, Expire: expire, LeaseType: typ, PrefixLen: plen, State: state}
	}
	for _, tc := range []struct {
		name string
		rows []Kea6Row
		typ  int
		want int
	}{
		{"a live lease", []Kea6Row{row("a", keaDUID, 600, 1500, 0, 128, 0)}, Kea6LeaseNA, 1},
		{"a renewal appends a row and counts once", []Kea6Row{
			row("a", keaDUID, 600, 1100, 0, 128, 0), row("a", keaDUID, 600, 1400, 0, 128, 0)}, Kea6LeaseNA, 1},
		{"the release row wins over the earlier live row", []Kea6Row{
			row("a", keaDUID, 600, 1500, 0, 128, 0), row("a", keaDUID, 0, 900, 0, 128, 0)}, Kea6LeaseNA, 0},
		{"an expired lease", []Kea6Row{row("a", keaDUID, 600, 999, 0, 128, 0)}, Kea6LeaseNA, 0},
		{"a declined lease", []Kea6Row{row("a", keaDUID, 600, 1500, 0, 128, 1)}, Kea6LeaseNA, 0},
		{"another client", []Kea6Row{row("a", "00:03:00:01:ff", 600, 1500, 0, 128, 0)}, Kea6LeaseNA, 0},
		{"the other lease type", []Kea6Row{row("p", keaDUID, 600, 1500, 2, 64, 0)}, Kea6LeaseNA, 0},
		{"a delegated prefix", []Kea6Row{row("p", keaDUID, 600, 1500, 2, 64, 0)}, Kea6LeasePD, 1},
		{"an address and its prefix are different leases", []Kea6Row{
			row("p", keaDUID, 600, 1500, 2, 64, 0), row("p", keaDUID, 0, 900, 0, 128, 0)}, Kea6LeasePD, 1},
		{"a release of one lease type does not hide the same address under another", []Kea6Row{
			row("a", keaDUID, 600, 1500, 0, 128, 0), row("a", keaDUID, 0, 900, 1, 128, 0)}, Kea6LeaseNA, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Kea6Held(tc.rows, strings.ToUpper(keaDUID), tc.typ, now); len(got) != tc.want {
				t.Errorf("held %d rows, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

func TestDUIDLLFromMAC(t *testing.T) {
	mac, _ := net.ParseMAC("02:42:ac:11:00:02")
	if got, want := DUIDLLFromMAC(mac), "00:03:00:01:02:42:ac:11:00:02"; got != want {
		t.Errorf("DUIDLLFromMAC = %q, want %q", got, want)
	}
}

// raFrameFrom is a parsed router advertisement the way the capture would hold it, from src.
func raFrameFrom(t *testing.T, src string, lifetime uint16) RAFrame {
	t.Helper()
	icmp := EncodeRA(RASpec{Managed: true, RouterLifetime: lifetime})
	frame := make([]byte, ethHeaderLen+ipv6HeaderLen, ethHeaderLen+ipv6HeaderLen+len(icmp))
	binary.BigEndian.PutUint16(frame[12:14], ethertypeIPv6)
	ip := frame[ethHeaderLen:]
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(icmp)))
	ip[6], ip[7] = protoICMPv6, 255
	a := netip.MustParseAddr(src).As16()
	copy(ip[8:24], a[:])
	frame = append(frame, icmp...)
	f, ok := ParseRA(frame)
	if !ok {
		t.Fatalf("the synthesised frame does not parse")
	}
	return f
}

func TestRASourceIP(t *testing.T) {
	f := raFrameFrom(t, "fe80::1234", 1800)
	if got, ok := RASourceIP(f); !ok || got != netip.MustParseAddr("fe80::1234") {
		t.Errorf("RASourceIP = %v %v, want fe80::1234", got, ok)
	}
	if _, ok := RASourceIP(RAFrame{Raw: make([]byte, 20)}); ok {
		t.Error("a truncated frame yielded a source address")
	}
}

func TestRouterAddressFindings(t *testing.T) {
	sender := []netip.Addr{netip.MustParseAddr("fe80::a")}
	server := []netip.Addr{netip.MustParseAddr("fe80::b")}
	fromSender := raFrameFrom(t, "fe80::a", 1800)
	fromKea := raFrameFrom(t, "fe80::b", 0)
	third := raFrameFrom(t, "fe80::c", 1800)
	thirdNotRouter := raFrameFrom(t, "fe80::c", 0)

	for _, tc := range []struct {
		name            string
		segment, onLink []RAFrame
		wantContains    string
	}{
		{"only the sender, seen on both links", []RAFrame{fromSender}, []RAFrame{fromSender}, ""},
		{"Kea advertises, even with lifetime zero", []RAFrame{fromSender, fromKea}, []RAFrame{fromSender}, "Kea's namespace sent"},
		{"Kea's frame seen only on its own link", []RAFrame{fromSender}, []RAFrame{fromSender, fromKea}, "server link: Kea's namespace sent"},
		{"a third router", []RAFrame{fromSender, third}, []RAFrame{fromSender}, "not the sender"},
		{"a third source that is not a router is no router address", []RAFrame{fromSender, thirdNotRouter}, []RAFrame{fromSender}, ""},
		{"the server link saw nothing from the sender", []RAFrame{fromSender}, nil, "positive control"},
		{"nothing captured at all", nil, nil, "positive control"},
		{"a frame with no readable source", []RAFrame{{Raw: []byte{1, 2, 3}}}, []RAFrame{fromSender}, "no readable source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(RouterAddressFindings(tc.segment, tc.onLink, sender, server), "\n")
			if tc.wantContains == "" && got != "" {
				t.Errorf("unexpected findings:\n%s", got)
			}
			if tc.wantContains != "" && !strings.Contains(got, tc.wantContains) {
				t.Errorf("findings %q do not contain %q", got, tc.wantContains)
			}
		})
	}
}

func TestKea6SocketFailure(t *testing.T) {
	if got := Kea6SocketFailure("INFO DHCP6_STARTED ok"); got != "" {
		t.Errorf("a clean start reported %q", got)
	}
	for _, m := range Kea6SocketFailures {
		if got := Kea6SocketFailure("ERROR " + m + " something"); got != m {
			t.Errorf("%s reported as %q", m, got)
		}
	}
}
