package analysis

import (
	"net/netip"
	"testing"
)

// ip is a test helper that parses a dotted-quad into a netip.Addr,
// failing the test on a bad literal.
func ip(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test IP %q: %v", s, err)
	}
	return addr
}

// anon is the zero-value netip.Addr, representing an anonymous (no
// response) observation.
var anon = netip.Addr{}

func TestDetectRouteChange_SustainedChangeFires(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")

	// Five consecutive sightings of b, hop currently displays a.
	obs := []netip.Addr{b, b, b, b, b}
	changed, newIP := DetectRouteChange(obs, a, 5)

	if !changed {
		t.Fatal("sustained change of 5 consecutive new IPs did not fire")
	}
	if newIP != b {
		t.Errorf("newIP = %v, want %v", newIP, b)
	}
}

func TestDetectRouteChange_BelowThresholdDoesNotFire(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")

	// Only four consecutive b; threshold is 5.
	obs := []netip.Addr{b, b, b, b, a}
	changed, _ := DetectRouteChange(obs, a, 5)

	if changed {
		t.Error("4 consecutive sightings fired with threshold 5; want no change")
	}
}

func TestDetectRouteChange_ECMPFlappingDoesNotFire(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")

	// Classic ECMP load-balancing: the hop alternates every probe.
	// Neither IP ever gets a consecutive run, so nothing should fire.
	obs := []netip.Addr{a, b, a, b, a, b, a, b}
	changed, _ := DetectRouteChange(obs, a, 3)

	if changed {
		t.Error("ECMP flapping fired a route change; want none")
	}
}

func TestDetectRouteChange_AnonymousEntriesAreSkipped(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")

	// b is genuinely stable but rate-limits its ICMP responses, so the
	// observation stream is interleaved with anonymous entries. Skipping
	// the anonymous entries, there are 5 consecutive b — it should fire.
	obs := []netip.Addr{b, anon, b, anon, b, anon, b, anon, b}
	changed, newIP := DetectRouteChange(obs, a, 5)

	if !changed {
		t.Fatal("rate-limited-but-stable hop did not fire; anonymous entries should be skipped, not break the run")
	}
	if newIP != b {
		t.Errorf("newIP = %v, want %v", newIP, b)
	}
}

func TestDetectRouteChange_DifferentIPBreaksRun(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")
	c := ip(t, "203.0.113.3")

	// Newest is b, but a c sits two back — the run of b is only 2 long.
	obs := []netip.Addr{b, b, c, b, b, b}
	changed, _ := DetectRouteChange(obs, a, 3)

	if changed {
		t.Error("a different valid IP mid-run should break the consecutive count")
	}
}

func TestDetectRouteChange_EmergesFromAnonymity(t *testing.T) {
	b := ip(t, "203.0.113.2")

	// The hop has never had an identity (currentIP is the zero value).
	// Once a stable IP is seen enough times, that is a change worth
	// recording — the hop now has an identity.
	obs := []netip.Addr{b, b, b}
	changed, newIP := DetectRouteChange(obs, anon, 3)

	if !changed {
		t.Fatal("hop emerging from anonymity did not fire")
	}
	if newIP != b {
		t.Errorf("newIP = %v, want %v", newIP, b)
	}
}

func TestDetectRouteChange_AllAnonymousReportsNothing(t *testing.T) {
	a := ip(t, "203.0.113.1")

	obs := []netip.Addr{anon, anon, anon, anon}
	changed, newIP := DetectRouteChange(obs, a, 1)

	if changed {
		t.Error("all-anonymous observations fired a change; want none")
	}
	if newIP.IsValid() {
		t.Errorf("newIP = %v, want invalid", newIP)
	}
}

func TestDetectRouteChange_SameAsCurrentDoesNotFire(t *testing.T) {
	a := ip(t, "203.0.113.1")

	// The hop is stably showing a, and a is also what it currently
	// displays. A confirmed run of the *current* IP is not a change.
	obs := []netip.Addr{a, a, a, a, a}
	changed, _ := DetectRouteChange(obs, a, 3)

	if changed {
		t.Error("a run of the current IP fired a change; want none")
	}
}

func TestDetectRouteChange_EmptyObservations(t *testing.T) {
	a := ip(t, "203.0.113.1")

	changed, newIP := DetectRouteChange(nil, a, 5)
	if changed || newIP.IsValid() {
		t.Errorf("empty observations: got (%v, %v), want (false, invalid)", changed, newIP)
	}
}

func TestDetectRouteChange_ThresholdClampedToOne(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")

	// A threshold below 1 is nonsensical; it must clamp to 1, not cause
	// every observation to fire or none to.
	obs := []netip.Addr{b}
	changed, newIP := DetectRouteChange(obs, a, 0)

	if !changed || newIP != b {
		t.Errorf("threshold 0 should clamp to 1: got (%v, %v), want (true, %v)", changed, newIP, b)
	}
}

func TestModalIP(t *testing.T) {
	a := ip(t, "203.0.113.1")
	b := ip(t, "203.0.113.2")
	c := ip(t, "203.0.113.3")

	tests := []struct {
		name   string
		obs    []netip.Addr
		want   netip.Addr
		wantOK bool
	}{
		{
			name:   "clear majority",
			obs:    []netip.Addr{a, a, a, b, c},
			want:   a,
			wantOK: true,
		},
		{
			name:   "anonymous entries ignored",
			obs:    []netip.Addr{anon, b, anon, b, anon, a},
			want:   b,
			wantOK: true,
		},
		{
			name: "tie broken toward more recent",
			// a and b each appear twice; a is newer (earlier in the
			// newest-first slice), so a wins.
			obs:    []netip.Addr{a, b, a, b},
			want:   a,
			wantOK: true,
		},
		{
			name:   "all anonymous",
			obs:    []netip.Addr{anon, anon},
			want:   netip.Addr{},
			wantOK: false,
		},
		{
			name:   "empty",
			obs:    nil,
			want:   netip.Addr{},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ModalIP(tt.obs)
			if ok != tt.wantOK {
				t.Errorf("ModalIP() ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("ModalIP() = %v, want %v", got, tt.want)
			}
		})
	}
}
