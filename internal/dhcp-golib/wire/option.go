package wire

import (
	"fmt"
	"sort"
)

// OptionCode is a DHCP option tag (RFC 2132).
type OptionCode uint8

// The option codes this milestone names. Everything else round-trips as raw
// bytes under its numeric code — the map is the authority, not this list.
const (
	OptPad                OptionCode = 0
	OptSubnetMask         OptionCode = 1
	OptTimeOffset         OptionCode = 2 // RFC 2132 section 3.4
	OptRouter             OptionCode = 3
	OptDNSServer          OptionCode = 6
	OptHostName           OptionCode = 12
	OptDomainName         OptionCode = 15
	OptInterfaceMTU       OptionCode = 26
	OptBroadcastAddress   OptionCode = 28
	OptStaticRoute        OptionCode = 33 // RFC 2132 section 5.8
	OptNTPServer          OptionCode = 42 // RFC 2132 section 8.3
	OptRequestedIP        OptionCode = 50
	OptLeaseTime          OptionCode = 51
	OptOverload           OptionCode = 52
	OptMessageType        OptionCode = 53
	OptServerID           OptionCode = 54
	OptParameterList      OptionCode = 55
	OptMessage            OptionCode = 56
	OptMaxMessageSize     OptionCode = 57
	OptRenewalTime        OptionCode = 58 // T1
	OptRebindingTime      OptionCode = 59 // T2
	OptVendorClassID      OptionCode = 60
	OptClientID           OptionCode = 61
	OptTFTPServer         OptionCode = 66  // RFC 2132 section 9.4
	OptBootfileName       OptionCode = 67  // RFC 2132 section 9.5
	OptFQDN               OptionCode = 81  // RFC 4702
	OptPosixTimezone      OptionCode = 100 // RFC 4833, PCode
	OptTZDatabase         OptionCode = 101 // RFC 4833, TCode
	OptDomainSearch       OptionCode = 119 // RFC 3397
	OptClasslessStaticRte OptionCode = 121 // RFC 3442
	// OptWPAD is the de-facto Web Proxy Auto-Discovery option. It sits in
	// RFC 2132's site-specific range and NO standards document defines it;
	// the name is what deployments call it, not what an RFC calls it. It is
	// named here so a decoded message reports "wpad" rather than
	// "option(252)", and this library gives it no meaning beyond the bytes.
	OptWPAD OptionCode = 252
	OptEnd  OptionCode = 255
)

func (c OptionCode) String() string {
	if n, ok := optionNames[c]; ok {
		return n
	}
	return fmt.Sprintf("option(%d)", uint8(c))
}

var optionNames = map[OptionCode]string{
	OptPad:                "pad",
	OptSubnetMask:         "subnet-mask",
	OptTimeOffset:         "time-offset",
	OptRouter:             "router",
	OptDNSServer:          "dns-server",
	OptHostName:           "host-name",
	OptDomainName:         "domain-name",
	OptInterfaceMTU:       "interface-mtu",
	OptBroadcastAddress:   "broadcast-address",
	OptStaticRoute:        "static-route",
	OptNTPServer:          "ntp-server",
	OptRequestedIP:        "requested-ip",
	OptLeaseTime:          "lease-time",
	OptOverload:           "overload",
	OptMessageType:        "message-type",
	OptServerID:           "server-id",
	OptParameterList:      "parameter-list",
	OptMessage:            "message",
	OptMaxMessageSize:     "max-message-size",
	OptRenewalTime:        "renewal-time",
	OptRebindingTime:      "rebinding-time",
	OptVendorClassID:      "vendor-class-id",
	OptClientID:           "client-id",
	OptTFTPServer:         "tftp-server",
	OptBootfileName:       "bootfile-name",
	OptFQDN:               "fqdn",
	OptPosixTimezone:      "posix-timezone",
	OptTZDatabase:         "tz-database",
	OptDomainSearch:       "domain-search",
	OptClasslessStaticRte: "classless-static-route",
	OptWPAD:               "wpad",
	OptEnd:                "end",
}

// Options holds every option in a message, keyed by code, values unparsed.
//
// Unparsed on purpose. The v1.x baseline exists because this project has
// already lost a field nobody wrote down; a pass-through map means a forgotten
// option is recoverable rather than gone (requirements section 9, choice 1).
type Options map[OptionCode][]byte

// Clone returns a deep copy. The codec hands decoded messages to ring 1, which
// is pure and must not be able to observe a later mutation of a slice the
// caller still holds.
func (o Options) Clone() Options {
	if o == nil {
		return nil
	}
	out := make(Options, len(o))
	for k, v := range o {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// Codes returns the option codes present, in ascending order.
//
// Sorted because encoding must be deterministic: Go map iteration is
// randomised, and a codec whose output depends on map order cannot have a
// golden-bytes test and cannot be replayed bit-exactly.
func (o Options) Codes() []OptionCode {
	out := make([]OptionCode, 0, len(o))
	for k := range o {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
