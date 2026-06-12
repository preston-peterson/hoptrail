package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// ErrTimeout is returned by Prober.Probe when no response arrives within
// the per-probe timeout.
var ErrTimeout = errors.New("probe timeout")

// ICMPProber is the narrow interface that probe loops (Discovery,
// Pinger) consume. The production implementation is *Prober (this
// package); tests substitute fakes that don't need real ICMP sockets.
//
// This is intentionally smaller than the *Prober method set — the
// loops only need Probe. Close, NewProber, etc. are owned by the cmd
// layer.
type ICMPProber interface {
	Probe(ctx context.Context, target netip.Addr, ttl uint8, timeout time.Duration) (Result, error)
}

// ReplyType classifies the kind of ICMP response received.
type ReplyType int

const (
	ReplyNone ReplyType = iota // zero-value sentinel (timeout / not set)
	ReplyEchoReply
	ReplyTimeExceeded
	ReplyDestUnreachable
)

// String returns a human-readable name for use in logs and the spike-
// style output line.
func (r ReplyType) String() string {
	switch r {
	case ReplyEchoReply:
		return "EchoReply"
	case ReplyTimeExceeded:
		return "TimeExceeded"
	case ReplyDestUnreachable:
		return "DestUnreachable"
	default:
		return "None"
	}
}

// Result is what Prober.Probe returns on a successful response.
type Result struct {
	RespIP netip.Addr
	RTT    time.Duration
	Type   ReplyType
}

// protocolICMP is IANA's assigned number for IPv4 ICMP (= 1). Used as
// the proto argument to icmp.ParseMessage.
const protocolICMP = 1

// Prober owns a raw ICMP socket and demultiplexes responses to in-flight
// probes by ICMP sequence number. Multiple goroutines may call Probe
// concurrently — the read loop matches each response to the goroutine
// that's waiting for it. This is the production replacement for the
// single-shot ProbeICMP function the spike used.
//
// Lifecycle: one Prober per probe engine. The engine's loops (discovery
// sweep, per-hop pinger) share it. Created at startup, closed on
// shutdown. Requires CAP_NET_RAW on the daemon's binary (granted via
// setcap at install).
type Prober struct {
	conn *icmp.PacketConn

	// id is this Prober's ICMP identifier, constant for the socket's
	// lifetime. Set to PID-low-bits per ping convention.
	id uint16

	mu      sync.Mutex
	nextSeq uint16                 // monotonic sequence counter (wraps)
	pending map[uint16]chan Result // seq → 1-buffer channel for the waiter
	closed  bool

	closeCh chan struct{}  // signals readLoop to exit
	wg      sync.WaitGroup // tracks readLoop
}

// NewProber opens a raw ICMP socket on all IPv4 interfaces and starts
// the response reader. The returned Prober is ready for concurrent
// Probe calls.
//
// Returns an error if the socket open fails — most commonly because the
// process lacks CAP_NET_RAW. See SECURITY.md for the privilege model.
func NewProber() (*Prober, error) {
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("open raw ICMP socket (needs CAP_NET_RAW — see SECURITY.md): %w", err)
	}
	p := &Prober{
		conn:    conn,
		id:      uint16(os.Getpid() & 0xffff),
		pending: make(map[uint16]chan Result),
		closeCh: make(chan struct{}),
	}
	p.wg.Add(1)
	go p.readLoop()
	return p, nil
}

// Close stops the read loop and closes the socket. Idempotent: calling
// Close more than once is safe and returns nil on subsequent calls.
//
// In-flight Probe calls return ErrTimeout once their per-probe timeout
// fires; Close does not interrupt them directly. The caller should
// ensure Probe callers are quiesced before relying on resource release.
func (p *Prober) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	close(p.closeCh)
	err := p.conn.Close()
	p.wg.Wait()
	return err
}

// Probe sends one ICMP Echo Request to target with the given IPv4 TTL
// and waits up to timeout for a response. Safe for concurrent calls;
// each call gets its own sequence number and waits on its own channel.
//
// Returns ErrTimeout if no response arrives in time. Returns ctx.Err()
// if the context is canceled. Otherwise returns the Result with the
// responding hop's IP and measured RTT.
//
// The TTL must be in 1..255. Target must be IPv4 (v0.1 is IPv4-only).
func (p *Prober) Probe(
	ctx context.Context,
	target netip.Addr,
	ttl uint8,
	timeout time.Duration,
) (Result, error) {
	if !target.Is4() {
		return Result{}, fmt.Errorf("target %s is not IPv4 (v0.1 is IPv4-only)", target)
	}
	if ttl == 0 {
		return Result{}, fmt.Errorf("ttl 0 is invalid")
	}

	seq, ch, err := p.register()
	if err != nil {
		return Result{}, err
	}
	defer p.unregister(seq)

	// SetTTL changes a socket-level option; serialize it with the send
	// so a concurrent Probe with a different TTL doesn't race the
	// kernel's WriteTo. Mutex held only across the syscall boundary,
	// which is microseconds — no real concurrency loss at our rates.
	p.mu.Lock()
	if err := p.conn.IPv4PacketConn().SetTTL(int(ttl)); err != nil {
		p.mu.Unlock()
		return Result{}, fmt.Errorf("set TTL: %w", err)
	}

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   int(p.id),
			Seq:  int(seq),
			Data: []byte("hoptrail"),
		},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		p.mu.Unlock()
		return Result{}, fmt.Errorf("marshal ICMP message: %w", err)
	}

	dst := &net.IPAddr{IP: target.AsSlice()}
	sent := time.Now()
	if _, err := p.conn.WriteTo(wire, dst); err != nil {
		p.mu.Unlock()
		return Result{}, fmt.Errorf("write probe: %w", err)
	}
	p.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		// readLoop fills RespIP and Type; we measure RTT here.
		res.RTT = time.Since(sent)
		return res, nil
	case <-timer.C:
		return Result{}, ErrTimeout
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// register reserves a unique sequence number and returns a 1-buffer
// channel the read loop will deliver to. The returned seq must be
// passed to unregister on completion.
func (p *Prober) register() (uint16, chan Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, nil, errors.New("prober closed")
	}
	// Look for a free seq starting at nextSeq. Wraps naturally at 65536.
	// Collisions in practice require thousands of probes in flight at
	// once, which our cadence doesn't produce.
	for i := 0; i < 65536; i++ {
		seq := p.nextSeq
		p.nextSeq++
		if _, busy := p.pending[seq]; !busy {
			ch := make(chan Result, 1)
			p.pending[seq] = ch
			return seq, ch, nil
		}
	}
	return 0, nil, errors.New("all ICMP sequence numbers are pending")
}

// unregister removes seq from the pending map. Safe to call after the
// channel has been delivered to or after a timeout.
func (p *Prober) unregister(seq uint16) {
	p.mu.Lock()
	delete(p.pending, seq)
	p.mu.Unlock()
}

// deliver hands a Result to whichever Probe call is waiting on seq.
// Non-blocking: if no one is waiting (probe already timed out, response
// arrived late), the result is silently dropped.
func (p *Prober) deliver(seq uint16, res Result) {
	p.mu.Lock()
	ch, ok := p.pending[seq]
	p.mu.Unlock()
	if !ok {
		return
	}
	// Channel is buffered with capacity 1; if it's already full (would
	// only happen if two responses matched the same seq, which the
	// kernel doesn't do), drop the extra.
	select {
	case ch <- res:
	default:
	}
}

// readLoop is the sole consumer of the ICMP socket. It runs as a single
// goroutine, started by NewProber, and exits when Close is called.
//
// The loop wakes periodically via a short read deadline so it can check
// closeCh. Without this, a graceful Close would block on ReadFrom until
// the next ICMP packet arrived (potentially forever).
func (p *Prober) readLoop() {
	defer p.wg.Done()
	buf := make([]byte, 1500)

	for {
		select {
		case <-p.closeCh:
			return
		default:
		}

		if err := p.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			return // socket likely closed
		}

		n, peer, err := p.conn.ReadFrom(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue // wake-up, not a real error
			}
			return // socket closed or fatal error
		}

		respIP, ok := ipFromPeer(peer)
		if !ok {
			continue
		}

		msg, err := icmp.ParseMessage(protocolICMP, buf[:n])
		if err != nil {
			continue // malformed packet, drop
		}

		seq, kind, ok := classifyReply(msg, p.id)
		if !ok {
			continue // not ours, or unknown type
		}

		// RTT is filled in by the Probe caller (it knows sent time).
		p.deliver(seq, Result{RespIP: respIP, Type: kind})
	}
}

// classifyReply extracts (seq, ReplyType) from a parsed ICMP message,
// filtering for responses that match our identifier and that we care
// about. Returns ok=false for messages we should ignore.
//
// Echo Reply: id is in the outer Echo body.
// Time Exceeded / Dest Unreachable: id and seq are in the inner ICMP
// header echoed inside the body's Data field.
func classifyReply(msg *icmp.Message, ourID uint16) (uint16, ReplyType, bool) {
	switch body := msg.Body.(type) {

	case *icmp.Echo:
		if msg.Type == ipv4.ICMPTypeEchoReply && body.ID == int(ourID) {
			return uint16(body.Seq), ReplyEchoReply, true
		}

	case *icmp.TimeExceeded:
		id, seq, ok := parseInnerEcho(body.Data)
		if ok && id == ourID {
			return seq, ReplyTimeExceeded, true
		}

	case *icmp.DstUnreach:
		id, seq, ok := parseInnerEcho(body.Data)
		if ok && id == ourID {
			return seq, ReplyDestUnreachable, true
		}
	}
	return 0, ReplyNone, false
}

// parseInnerEcho extracts the original (id, seq) from the inner packet
// embedded in an ICMP error message (Time Exceeded, Dest Unreachable).
//
// Wire layout of the body's Data:
//
//	[0..IHL*4)        : inner IPv4 header (variable length per IHL)
//	[IHL*4..IHL*4+8)  : first 8 bytes of inner ICMP header
//	                     offset 0: type (1 byte; must be 8 for Echo)
//	                     offset 1: code (1 byte)
//	                     offset 2-3: checksum (2 bytes)
//	                     offset 4-5: id (2 bytes, big-endian)
//	                     offset 6-7: sequence (2 bytes, big-endian)
//
// Returns ok=false for malformed input or for inner packets that aren't
// our Echo Request (e.g. the response is in reply to some other tool's
// traffic and arrived on our raw socket).
func parseInnerEcho(data []byte) (id uint16, seq uint16, ok bool) {
	if len(data) < 20 {
		return 0, 0, false
	}
	// Bottom 4 bits of byte 0 are IHL; multiply by 4 to get header length.
	ihl := int(data[0]&0x0f) * 4
	if ihl < 20 || len(data) < ihl+8 {
		return 0, 0, false
	}
	icmpHdr := data[ihl:]
	// Type 8 = Echo Request — only match what we sent.
	if icmpHdr[0] != 8 {
		return 0, 0, false
	}
	id = binary.BigEndian.Uint16(icmpHdr[4:6])
	seq = binary.BigEndian.Uint16(icmpHdr[6:8])
	return id, seq, true
}

// ipFromPeer extracts a netip.Addr from a net.Addr returned by ReadFrom.
// Raw ICMP sockets return *net.IPAddr; the helper also accepts
// *net.UDPAddr defensively.
func ipFromPeer(peer net.Addr) (netip.Addr, bool) {
	switch p := peer.(type) {
	case *net.IPAddr:
		addr, ok := netip.AddrFromSlice(p.IP.To4())
		if !ok {
			return netip.Addr{}, false
		}
		return addr.Unmap(), true
	case *net.UDPAddr:
		addr, ok := netip.AddrFromSlice(p.IP.To4())
		if !ok {
			return netip.Addr{}, false
		}
		return addr.Unmap(), true
	default:
		return netip.Addr{}, false
	}
}
