package probe

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// makeInnerEcho builds a synthetic "inner packet" body for a Time
// Exceeded or Dest Unreachable response: 20 bytes of IPv4 header
// followed by 8 bytes of ICMP Echo header carrying the given id and
// sequence. The IHL is fixed at 5 (no options), so the IP header is
// exactly 20 bytes.
//
// This is the byte layout the kernel constructs when a router decrements
// our TTL to zero and echoes our original packet back inside its Time
// Exceeded response. parseInnerEcho's job is to pull (id, seq) out of
// this structure.
func makeInnerEcho(icmpType uint8, id, seq uint16) []byte {
	buf := make([]byte, 28)
	// IPv4 header (minimum, 20 bytes). Only IHL matters for our parser.
	buf[0] = 0x45 // version 4, IHL 5 (= 5 × 4 = 20 bytes)
	// bytes [1..20) are header fields we don't care about for parsing.

	// Inner ICMP header at offset 20.
	buf[20] = icmpType // Echo Request type = 8
	buf[21] = 0        // code
	// checksum at [22:24] left zero (parser doesn't verify it)
	binary.BigEndian.PutUint16(buf[24:26], id)
	binary.BigEndian.PutUint16(buf[26:28], seq)
	return buf
}

func TestParseInnerEcho_ValidEchoRequest(t *testing.T) {
	const wantID, wantSeq = uint16(0xABCD), uint16(0x1234)
	body := makeInnerEcho(8, wantID, wantSeq)

	id, seq, ok := parseInnerEcho(body)
	if !ok {
		t.Fatal("parseInnerEcho returned ok=false for valid Echo Request inner packet")
	}
	if id != wantID || seq != wantSeq {
		t.Errorf("parseInnerEcho = (id=%#x, seq=%#x), want (id=%#x, seq=%#x)", id, seq, wantID, wantSeq)
	}
}

func TestParseInnerEcho_TruncatedReturnsFalse(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		make([]byte, 19),            // less than IPv4 header
		makeInnerEcho(8, 1, 1)[:25], // valid prefix but missing the seq bytes
	}
	for i, body := range cases {
		if _, _, ok := parseInnerEcho(body); ok {
			t.Errorf("case %d: parseInnerEcho(len=%d) returned ok=true; want false", i, len(body))
		}
	}
}

func TestParseInnerEcho_WrongInnerICMPTypeReturnsFalse(t *testing.T) {
	// Inner ICMP type = 0 (Echo Reply) — that's not what we sent, so
	// we should ignore it. parseInnerEcho only matches type 8.
	body := makeInnerEcho(0, 1, 1)
	if _, _, ok := parseInnerEcho(body); ok {
		t.Error("parseInnerEcho returned ok=true for inner type=0; want false (only Echo Request matches)")
	}
}

func TestParseInnerEcho_LongerIHLOK(t *testing.T) {
	// Build a body with IHL=6 (24-byte IPv4 header with 4 bytes of
	// options), then the inner ICMP header. parseInnerEcho should use
	// IHL to find the ICMP header, not hardcode offset 20.
	body := make([]byte, 32)
	body[0] = 0x46                                  // version 4, IHL 6 (= 24 bytes)
	body[24] = 8                                    // ICMP type Echo Request
	binary.BigEndian.PutUint16(body[28:30], 0xCAFE) // id
	binary.BigEndian.PutUint16(body[30:32], 0xBEEF) // seq

	id, seq, ok := parseInnerEcho(body)
	if !ok {
		t.Fatal("parseInnerEcho returned ok=false for valid IHL=6 inner packet")
	}
	if id != 0xCAFE || seq != 0xBEEF {
		t.Errorf("parseInnerEcho = (id=%#x, seq=%#x), want (id=0xCAFE, seq=0xBEEF)", id, seq)
	}
}

func TestParseInnerEcho_InvalidIHLReturnsFalse(t *testing.T) {
	// IHL < 5 is impossible per the IPv4 spec; parser should reject.
	body := make([]byte, 28)
	body[0] = 0x44 // version 4, IHL 4 (invalid, < 5)
	if _, _, ok := parseInnerEcho(body); ok {
		t.Error("parseInnerEcho accepted IHL=4; want rejection")
	}
}

func TestClassifyReply_EchoReplyMatchesOurID(t *testing.T) {
	const ourID = uint16(0xABCD)
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: int(ourID), Seq: 42},
	}
	seq, kind, ok := classifyReply(msg, ourID)
	if !ok || kind != ReplyEchoReply || seq != 42 {
		t.Errorf("classifyReply = (seq=%d, kind=%v, ok=%v); want (42, EchoReply, true)", seq, kind, ok)
	}
}

func TestClassifyReply_EchoReplyWrongIDIgnored(t *testing.T) {
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: 0x1111, Seq: 42},
	}
	if _, _, ok := classifyReply(msg, 0xABCD); ok {
		t.Error("classifyReply matched Echo Reply with wrong ID; want it to be ignored")
	}
}

func TestClassifyReply_EchoRequestIgnored(t *testing.T) {
	// Our raw socket sees outgoing Echo Requests from other processes
	// (and our own, on some configurations). They are not responses to
	// our probes; classifyReply must skip them.
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho, // not EchoReply
		Body: &icmp.Echo{ID: 0xABCD, Seq: 42},
	}
	if _, _, ok := classifyReply(msg, 0xABCD); ok {
		t.Error("classifyReply matched an Echo Request; want it ignored (only EchoReply matters)")
	}
}

func TestClassifyReply_TimeExceededExtractsInnerSeq(t *testing.T) {
	const ourID = uint16(0xABCD)
	const wantSeq = uint16(7)
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeTimeExceeded,
		Body: &icmp.TimeExceeded{Data: makeInnerEcho(8, ourID, wantSeq)},
	}
	seq, kind, ok := classifyReply(msg, ourID)
	if !ok || kind != ReplyTimeExceeded || seq != wantSeq {
		t.Errorf("classifyReply = (seq=%d, kind=%v, ok=%v); want (%d, TimeExceeded, true)", seq, kind, ok, wantSeq)
	}
}

func TestClassifyReply_TimeExceededOtherIDIgnored(t *testing.T) {
	// Time Exceeded from someone else's traceroute lands on our raw
	// socket too; must be filtered by the inner ID.
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeTimeExceeded,
		Body: &icmp.TimeExceeded{Data: makeInnerEcho(8, 0x1111, 7)},
	}
	if _, _, ok := classifyReply(msg, 0xABCD); ok {
		t.Error("classifyReply matched a Time Exceeded with foreign ID; want ignored")
	}
}

func TestClassifyReply_DestUnreachableExtractsInnerSeq(t *testing.T) {
	const ourID = uint16(0xABCD)
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeDestinationUnreachable,
		Body: &icmp.DstUnreach{Data: makeInnerEcho(8, ourID, 9)},
	}
	seq, kind, ok := classifyReply(msg, ourID)
	if !ok || kind != ReplyDestUnreachable || seq != 9 {
		t.Errorf("classifyReply = (seq=%d, kind=%v, ok=%v); want (9, DestUnreachable, true)", seq, kind, ok)
	}
}
