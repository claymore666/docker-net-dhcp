package lease

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// The v6 manager's own arms: what arrives on the two v6 ports, and what the
// v6 machine's action kinds mean here.
//
// THEY SIT IN A FILE OF THEIR OWN AND NOT INSIDE manager.go's SWITCHES for the
// reason newManager6 is its own function: interleaving them would produce
// functions in which half the lines are dead for any given caller. What is
// SHARED is the loop — dispatch, drain, emit, the counters, the ordering rule
// that an action failure is queued rather than fed immediately — because that
// loop is the same problem in both families and duplicating it would be two
// serialisation points to keep in agreement.

// onInbound6 is one DHCPv6 payload off the transport.
func (mg *Manager) onInbound6(ctx context.Context, in Inbound) {
	if in.Err != nil {
		mg.bump(func(s *Stats) { s.TransportErrors++ })
		mg.packets6.Record(CapturedPacketV6{
			At: mg.cfg.Clock.Wall(), Dir: DirIn, DecodeErr: in.Err,
		})
		return
	}
	mg.bump(func(s *Stats) { s.Received++ })
	msg, err := wire.DecodeV6(in.Payload)
	mg.packets6.Record(CapturedPacketV6{
		At:        mg.cfg.Clock.Wall(),
		Dir:       DirIn,
		Raw:       append([]byte(nil), in.Payload...),
		Msg:       msg,
		DecodeErr: err,
	})
	if err != nil {
		// A payload that will not decode never reaches ring 1, and it is
		// counted and captured for the v4 path's reason: "we dropped
		// something" with no evidence is the failure this library's debug
		// requirements exist for.
		mg.bump(func(s *Stats) { s.DecodeFailures++ })
		return
	}
	mg.dispatch(ctx, proto.ReceivedV6(msg, append([]byte(nil), in.Payload...)))
}

// onND is one ICMPv6 Neighbor Discovery frame off the link.
//
// ONLY A ROUTER ADVERTISEMENT REACHES RING 1 FROM HERE. RFC 4861 defines five
// message types on this socket and the v6 machine consumes exactly one of
// them: the Neighbor Solicitations and Advertisements of RFC 4862 section
// 5.4's duplicate address detection are ring 3's exchange (M7c), reported up
// as a proto.EvDADResult rather than as frames, and a Redirect is a routing
// message this library has no opinion about.
//
// THE THREE REFUSALS ARE COUNTED SEPARATELY, for onARP's reason: a link
// carries Neighbor Discovery continuously, every event that reaches Step costs
// a journal entry, and the journal is bounded — so an unfiltered feed would
// wrap it between one acquisition and the next.
func (mg *Manager) onND(ctx context.Context, in NDInbound) {
	if in.Err != nil {
		mg.bump(func(s *Stats) { s.NDErrors++ })
		return
	}
	// ONE bump per frame, carrying both the sighting and its verdict, for the
	// reason onARP gives: two bumps leave a window in which a reader sees the
	// frame counted and not yet classified.
	ra, err := wire.DecodeRouterAdvert(in.Frame)
	if err != nil {
		// Not a Router Advertisement. That covers both "a Neighbor
		// Advertisement, which is not ours to read" and "not a valid ICMPv6
		// message at all", and the two are not separated here because this
		// ring cannot tell them apart without a second decode of a frame it
		// is going to drop either way. What distinguishes them is the type
		// octet, and it is in the frame the caller still has.
		mg.bump(func(s *Stats) { s.NDSeen++; s.NDIgnored++ })
		return
	}
	mg.bump(func(s *Stats) { s.NDSeen++; s.RouterAdvertsSeen++ })
	mg.packets6.Record(CapturedPacketV6{
		At:  mg.cfg.Clock.Wall(),
		Dir: DirIn,
		Raw: append([]byte(nil), in.Frame...),
		RA:  ra,
	})
	mg.dispatch(ctx, proto.RouterAdvertRaw(ra, append([]byte(nil), in.Frame...)))
}

// sendV6 is proto.ActSendV6, through RFC 9915 section 14.1's bucket.
//
// A REFUSED SEND IS AN ACTION FAILURE, NOT A SILENT DROP. R2 is that the
// machine is told when an action did not happen, and it holds here for a
// stronger reason than usual: the whole point of section 14.1 is to break a
// loop in which the client keeps starting exchanges, and a client that
// believed its message went out would arm a retransmission timer and come back
// for another token. Told the send failed, it counts the failure against
// Params6.MaxSendFailures and stops.
func (mg *Manager) sendV6(a proto.Action, bridgeAt clockBridge) *proto.Event {
	raw, err := wire.EncodeV6(a.MsgV6)
	if err != nil {
		mg.bump(func(s *Stats) { s.SendFailures++ })
		ev := proto.ActionFailed(a.ID, "encode: "+err.Error())
		return &ev
	}
	if !mg.limiter.take(mg.cfg.Clock.Mono()) {
		mg.bump(func(s *Stats) { s.RateLimited++; s.SendFailures++ })
		// ONE LINE, NOT TWO. §14.1's refusal reaches the journal as the
		// ActionFailed the machine is told about, and that entry is a Step:
		// it replays, it is ordered against the send it refused, and its
		// Reason is this sentence. A journalNote beside it wrote the same
		// sentence a second time — M-3, one fact derived twice — and the
		// duplicate is what made deleting either copy invisible. MEASURED:
		// removing the journalNote SURVIVED the suite.
		note := fmt.Sprintf("RFC 9915 §14.1 rate limit: %s refused, %d refused on this interface so far (limit %d per %s)",
			a.MsgV6.Type, mg.limiter.Refused(), mg.limiter.limit.Messages, mg.limiter.limit.Interval)
		ev := proto.ActionFailed(a.ID, note)
		return &ev
	}
	if err := mg.cfg.TransportV6.Send(a.Dest, raw); err != nil {
		mg.bump(func(s *Stats) { s.SendFailures++ })
		ev := proto.ActionFailed(a.ID, err.Error())
		return &ev
	}
	mg.bump(func(s *Stats) { s.Sent++ })
	mg.countSent6(a.MsgV6)
	mg.packets6.Record(CapturedPacketV6{
		At: bridgeAt.wall, Dir: DirOut, Raw: raw, Msg: a.MsgV6,
	})
	return nil
}

// sendRouterSolicit is proto.ActSendRouterSolicit.
//
// IT DOES NOT PASS THROUGH THE SECTION 14.1 BUCKET. Section 14.1 bounds "DHCP
// messages", and a Router Solicitation is not one: it is RFC 4861 section
// 4.1's ICMPv6 message on a different socket, already bounded by section
// 6.3.7's MAX_RTR_SOLICITATIONS. Putting it in the same bucket would let
// router discovery spend the budget the DHCP exchange needs.
func (mg *Manager) sendRouterSolicit(a proto.Action, bridgeAt clockBridge) *proto.Event {
	src := mg.cfg.LinkLocal
	if !src.Is6() || src.Is4In6() {
		// RFC 4861 section 4.1's other legal source: "the unspecified address
		// if no address is assigned to the sending interface".
		src = netip.IPv6Unspecified()
	}
	hw := mg.cfg.LinkHW
	if src.IsUnspecified() {
		// Section 4.1: the Source Link-Layer Address option "MUST NOT be
		// included if the Source Address is the unspecified address."
		hw = nil
	}
	pkt, err := wire.EncodeRouterSolicit(src, hw)
	if err != nil {
		mg.bump(func(s *Stats) { s.NDSendFailures++ })
		ev := proto.ActionFailed(a.ID, "encode router solicitation: "+err.Error())
		return &ev
	}
	if err := mg.cfg.ND.Send(pkt); err != nil {
		mg.bump(func(s *Stats) { s.NDSendFailures++ })
		ev := proto.ActionFailed(a.ID, err.Error())
		return &ev
	}
	mg.bump(func(s *Stats) { s.RouterSolicitsSent++ })
	mg.packets6.Record(CapturedPacketV6{
		At: bridgeAt.wall, Dir: DirOut, Raw: append([]byte(nil), pkt.Body...), RS: true,
	})
	return nil
}

// countSent6 is countSent's counterpart: the per-message-type counters, read
// off the message that actually left rather than off the machine's intention.
func (mg *Manager) countSent6(msg *wire.MessageV6) {
	switch msg.Type {
	case wire.MsgRenew, wire.MsgRebind:
		mg.bump(func(s *Stats) { s.RenewalsSent++ })
	case wire.MsgDecline6:
		mg.bump(func(s *Stats) { s.DeclinesSent++ })
	case wire.MsgRelease6:
		mg.bump(func(s *Stats) { s.ReleasesSent++ })
	}
}
