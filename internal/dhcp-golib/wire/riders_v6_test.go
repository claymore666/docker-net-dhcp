package wire

import (
	"errors"
	"net/netip"
	"testing"
)

// TestICMPCodeIsPartOfValidity drives the M7a review's finding 1: neither
// ICMPv6 decoder read the Code octet, so an RA or NA written to a protocol
// this package does not implement decoded and reported its flags.
//
// RFC 4861 §6.1.2 puts "ICMP Code is 0" in the same MUST-silently-discard list
// as the two checks these decoders already enforce, and adds the reason:
// "backward-incompatible changes may use different Code values." §7.1.2
// repeats it for a Neighbor Advertisement.
//
// THE CAPTURED FRAMES ARE THE PRESERVATION CONTROL. Every frame this package
// recorded off a real link carries Code 0, so the check cannot be a refusal of
// ordinary traffic; the rows below flip exactly that one octet in exactly
// those frames.
func TestICMPCodeIsPartOfValidity(t *testing.T) {
	withCode := func(b []byte, code uint8) []byte {
		out := append([]byte(nil), b...)
		out[1] = code
		return out
	}

	t.Run("router advertisement", func(t *testing.T) {
		if _, err := DecodeRouterAdvert(capRouterAdvert); err != nil {
			t.Fatalf("the captured advertisement was refused with Code 0: %v", err)
		}
		for _, code := range []uint8{1, 2, 0xFF} {
			ra, err := DecodeRouterAdvert(withCode(capRouterAdvert, code))
			if !errors.Is(err, ErrICMPv6Validity) {
				t.Errorf("Code %d: %v, want %v", code, err, ErrICMPv6Validity)
			}
			if ra != nil {
				t.Errorf("Code %d: a refused decode still reported M=%t O=%t", code, ra.Managed, ra.Other)
			}
		}
	})

	t.Run("neighbor advertisement", func(t *testing.T) {
		if _, err := DecodeNeighborAdvert(capNeighborAdvert); err != nil {
			t.Fatalf("the captured advertisement was refused with Code 0: %v", err)
		}
		for _, code := range []uint8{1, 2, 0xFF} {
			na, err := DecodeNeighborAdvert(withCode(capNeighborAdvert, code))
			if !errors.Is(err, ErrICMPv6Validity) {
				t.Errorf("Code %d: %v, want %v", code, err, ErrICMPv6Validity)
			}
			if na != nil {
				t.Errorf("Code %d: a refused decode still reported target %s", code, na.Target)
			}
		}
	})
}

// TestNeighborAdvertTargetIsNotMulticast is §7.1.2's "Target Address is not a
// multicast address", the other unread check of the same list.
//
// It is the one on that list a CONSUMER can act on wrongly rather than merely
// parse: duplicate address detection reads the target to decide whether an
// advertisement answers its own probe, and a multicast target is an answer
// about nobody. Ring 1 compares the target to the address it asked about, so
// the defect would be silent there too — the refusal belongs here.
func TestNeighborAdvertTargetIsNotMulticast(t *testing.T) {
	withTarget := func(a netip.Addr) []byte {
		out := append([]byte(nil), capNeighborAdvert...)
		v := a.As16()
		copy(out[8:24], v[:])
		return out
	}
	for _, tc := range []struct {
		name   string
		target string
		want   error
	}{
		{"the captured unicast target", "fe80::e849:4eff:fee5:31ed", nil},
		{"a global unicast target", "fd00:99::183", nil},
		{"all-nodes", "ff02::1", ErrICMPv6Validity},
		{"a solicited-node group", "ff02::1:ff00:5150", ErrICMPv6Validity},
		{"an interface-local group", "ff01::1", ErrICMPv6Validity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			na, err := DecodeNeighborAdvert(withTarget(addr(t, tc.target)))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("DecodeNeighborAdvert: %v", err)
				}
				if na.Target.String() != tc.target {
					t.Errorf("target %s, want %s", na.Target, tc.target)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("DecodeNeighborAdvert = %v, %v; want %v", na, err, tc.want)
			}
			if na != nil {
				t.Errorf("a refused decode returned an advertisement: %v", na)
			}
		})
	}
}

// TestPreferenceReadsItsOwnOctet is §21.8's Preference option, which M7a did
// not decode and §18.2.9's server selection cannot be written without.
//
// THE CAPTURED ADVERTISE IS THE OUTSIDE EVIDENCE. dnsmasq's own log line for
// that exchange reads "sent size: 1 option: 7 preference 255", so the value
// asserted below is the server's account of what it built and not this
// package's reading of its own bytes. 255 is also the value §18.2.9 gives its
// one short-circuit to, which makes the captured frame the interesting case
// rather than a convenient one.
func TestPreferenceReadsItsOwnOctet(t *testing.T) {
	msg, err := DecodeV6(capAdvertise)
	if err != nil {
		t.Fatalf("DecodeV6(capAdvertise): %v", err)
	}
	pref, ok, err := msg.Options.Preference()
	if err != nil || !ok {
		t.Fatalf("Preference() = %d, %t, %v; want the captured 255", pref, ok, err)
	}
	if pref != 255 {
		t.Fatalf("preference %d, want 255 — dnsmasq logged \"option: 7 preference 255\"", pref)
	}

	// The Reply of the same exchange carries no Preference option, which is
	// §21.8's "Absence of option means preference 0" — and the boolean is what
	// separates that from a server that sent an explicit 0.
	rep, err := DecodeV6(capReply)
	if err != nil {
		t.Fatalf("DecodeV6(capReply): %v", err)
	}
	if pref, ok, err := rep.Options.Preference(); pref != 0 || ok || err != nil {
		t.Fatalf("Preference() on the Reply = %d, %t, %v; want 0, false, nil", pref, ok, err)
	}

	for _, tc := range []struct {
		name string
		data []byte
		want uint8
		ok   bool
		err  error
	}{
		{"an explicit zero", []byte{0x00}, 0, true, nil},
		{"one", []byte{0x01}, 1, true, nil},
		{"254", []byte{0xFE}, 254, true, nil},
		{"255", []byte{0xFF}, 255, true, nil},
		{"empty", []byte{}, 0, true, ErrV6BadOption},
		{"two octets", []byte{0xFF, 0x00}, 0, true, ErrV6BadOption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := OptionsV6{{Code: OptV6Preference, Data: tc.data}}
			pref, ok, err := o.Preference()
			if !errors.Is(err, tc.err) {
				t.Fatalf("error %v, want %v", err, tc.err)
			}
			if ok != tc.ok {
				t.Errorf("found = %t, want %t", ok, tc.ok)
			}
			if pref != tc.want {
				t.Errorf("preference %d, want %d", pref, tc.want)
			}
		})
	}
}

// TestStatusMalformedIsNotSuccess is the M7a review's finding 4: Status()
// returned Status{} beside ErrV6BadOption, and Status{} is byte-identical to
// Success — defeat row A-4's own defect shape surviving in the VALUE after the
// error had separated it.
//
// The owning test discarded the value with `_, _, err :=`; this one asserts
// it, which is the whole difference.
func TestStatusMalformedIsNotSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"one octet", []byte{0x00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := OptionsV6{{Code: OptV6StatusCode, Data: tc.data}}
			s, found, err := o.Status()
			if !errors.Is(err, ErrV6BadOption) {
				t.Fatalf("error %v, want %v", err, ErrV6BadOption)
			}
			if !found {
				t.Error("found = false: the option WAS present, malformed")
			}
			if s.Code == StatusSuccess {
				t.Fatal("the malformed value is byte-identical to Success, so a caller that read the value and dropped the error was told the server said Success")
			}
			if s.Code != StatusMalformed {
				t.Errorf("code %d, want StatusMalformed", uint16(s.Code))
			}
			if s.String() != "malformed" {
				t.Errorf("String() = %q, want %q", s.String(), "malformed")
			}
		})
	}

	// The preservation control: the two shapes that are NOT malformed keep
	// their answers, so the sentinel has not swallowed the ordinary cases.
	for _, tc := range []struct {
		name  string
		opts  OptionsV6
		code  StatusCode
		found bool
	}{
		{"absent", OptionsV6{}, StatusSuccess, false},
		{"an explicit Success", OptionsV6{{Code: OptV6StatusCode, Data: []byte{0, 0}}}, StatusSuccess, true},
		{"NoAddrsAvail", OptionsV6{{Code: OptV6StatusCode, Data: []byte{0, 2}}}, StatusNoAddrsAvail, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, found, err := tc.opts.Status()
			if err != nil {
				t.Fatalf("Status(): %v", err)
			}
			if s.Code != tc.code || found != tc.found {
				t.Errorf("Status() = %v, %t; want %v, %t", s.Code, found, tc.code, tc.found)
			}
		})
	}
}
