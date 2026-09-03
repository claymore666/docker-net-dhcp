// Package wire is ring 0: the codec. Bytes to typed messages and back.
//
// Ring 0 is pure. It holds no clock, opens no socket and touches no ambient
// state; it is subject to the same import policy as ring 1 (see the T1 gate,
// internal/gates/t1), because ring 1 imports it and an impure ring 0 would
// make ring 1 impure transitively.
//
// DHCPv4 only, and only what DISCOVER / OFFER / REQUEST / ACK / NAK need. The
// message struct carries every BOOTP field and every option, so a message
// carrying options this milestone does not interpret round-trips without loss.
// That is why Options is a byte map and not a struct of parsed fields.
//
// The decoder is defensive about a truncated option header, an option whose
// length runs past the buffer, a message shorter than the fixed header, a
// missing END option, a bad magic cookie, option overload into 'file' and
// 'sname' (RFC 2131 section 4.1), and a repeated option code, whose values are
// concatenated (RFC 2131 section 4.1, RFC 3396).
//
// BOUND: it does not bound the total number of options — the input is one
// datagram the caller has already sized — and it does not reject a
// semantically impossible message such as an OFFER with no yiaddr, because
// whether a message is usable depends on the state the machine is in.
//
// D3 (own codec versus insomniacslk/dhcp) is open. This exists because the T1
// gate refuses a third-party import in a pure ring and the four messages M1
// needs are a few hundred lines. If D3 lands on the external library, ring 0
// is replaceable behind rings 1-3 — but the gate has to be argued with, not
// edited around.
package wire
