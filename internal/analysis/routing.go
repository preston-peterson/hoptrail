package analysis

import "net/netip"

// DetectRouteChange decides whether the IP seen at a single hop (TTL) has
// genuinely changed, as opposed to merely flapping due to ECMP load
// balancing.
//
// observations is the recent IP-at-TTL sightings for one hop, ordered
// newest-first (exactly what RingBuffer.NewestFirst returns). An invalid
// netip.Addr (the zero value) represents an anonymous observation — a
// probe where the hop did not respond.
//
// currentIP is the IP the hop is currently displaying as its identity
// (the zero value if the hop has been anonymous so far).
//
// threshold is how many consecutive sightings of a new IP are required
// before the change is believed. This is the ECMP-bucketing knob.
//
// The rule: walk observations newest-first, skipping anonymous entries
// (they neither confirm nor break a run). Count how many times the most
// recent non-anonymous IP appears consecutively. If that count reaches
// threshold AND the IP differs from currentIP, report a change.
//
// Why skip anonymous entries instead of letting them break the run: many
// routers rate-limit their ICMP responses, so a hop that is genuinely
// stable still produces intermittent non-responses. Treating those as
// run-breakers would make route changes nearly impossible to detect on
// any rate-limited hop. Skipping them counts only what was actually seen.
//
// Behavior worth noting:
//   - Pure ECMP flapping (A,B,A,B,A,B newest-first) never reaches a
//     consecutive run of threshold for either IP, so nothing fires.
//   - A sustained change (B,B,B,B,B with currentIP=A) fires once the run
//     hits threshold.
//   - A hop emerging from anonymity (B,B,B,B,B with currentIP invalid)
//     fires, because B differs from the zero-value currentIP. The caller
//     gets to record "the hop now has an identity".
//   - All-anonymous observations report no change.
func DetectRouteChange(
	observations []netip.Addr,
	currentIP netip.Addr,
	threshold int,
) (changed bool, newIP netip.Addr) {
	if threshold < 1 {
		threshold = 1
	}

	// Find the most recent non-anonymous observation; that is the
	// candidate identity for this hop.
	var candidate netip.Addr
	found := false
	for _, obs := range observations {
		if obs.IsValid() {
			candidate = obs
			found = true
			break
		}
	}
	if !found {
		// Every recent observation was anonymous. Nothing to conclude.
		return false, netip.Addr{}
	}

	// Count consecutive sightings of candidate, newest-first, skipping
	// anonymous entries. A different valid IP breaks the run.
	consecutive := 0
	for _, obs := range observations {
		switch {
		case !obs.IsValid():
			// Anonymous: neither confirms nor breaks. Skip.
			continue
		case obs == candidate:
			consecutive++
		default:
			// A different valid IP — the consecutive run ends here.
			goto done
		}
	}
done:

	if consecutive >= threshold && candidate != currentIP {
		return true, candidate
	}
	return false, netip.Addr{}
}

// ModalIP returns the IP that appears most often across observations,
// ignoring anonymous (invalid) entries. It is the value a hop should
// display as its identity between confirmed route changes.
//
// Ties are broken toward the more recent IP: because observations are
// newest-first, the first IP to reach the winning count keeps it. ok is
// false when every observation is anonymous.
func ModalIP(observations []netip.Addr) (modal netip.Addr, ok bool) {
	counts := make(map[netip.Addr]int)
	for _, obs := range observations {
		if obs.IsValid() {
			counts[obs]++
		}
	}
	if len(counts) == 0 {
		return netip.Addr{}, false
	}

	best := 0
	// Iterate observations (not the map) so tie-breaking is deterministic
	// and recency-biased rather than dependent on map iteration order.
	for _, obs := range observations {
		if !obs.IsValid() {
			continue
		}
		if counts[obs] > best {
			best = counts[obs]
			modal = obs
		}
	}
	return modal, true
}
