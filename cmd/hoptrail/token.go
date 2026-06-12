// `hoptrail token` — probe-auth token utilities for v0.3 distributed
// probing (docs/v0.3-protocol-design.md §6). The only verb is `gen`:
// generate one opaque shared secret, which the operator pastes into
// the central's probes.tokens list and each remote probe's config.

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
)

// agentTokenBytes is the entropy of a generated probe token. 32 bytes
// (256 bits) base64-encodes to 43 chars — far past any brute-force
// horizon and comfortably above the config layer's length floor.
const agentTokenBytes = 32

// cmdToken implements `hoptrail token <verb>`. Returns the process
// exit code.
func cmdToken(args []string) int {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" {
		tokenUsage()
		return 2
	}
	switch args[0] {
	case "gen":
		return cmdTokenGen(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "hoptrail: unknown token verb %q\n\n", args[0])
		tokenUsage()
		return 2
	}
}

func tokenUsage() {
	fmt.Fprint(os.Stderr, `usage:
  hoptrail token gen    generate a probe bearer token

NOTE: the easier path is the central's web UI (Settings -> Probes ->
Add probe), which mints a token and prints the probe's full install
command — no config edits, no restart. This command remains for the
yaml-managed flow.

The token is printed to stdout (everything else goes to stderr), so
it can be piped or redirected. Add it to the central's config:

  probes:
    tokens:
      - "<token>"

and to the remote probe's config:

  central:
    token: "<token>"

then restart both. Revoke by removing it from the central's list.
`)
}

// cmdTokenGen implements `hoptrail token gen`.
func cmdTokenGen(args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "hoptrail: token gen takes no arguments\n")
		return 2
	}
	token, err := generateToken(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	// Token alone on stdout; the placement reminder on stderr so
	// `hoptrail token gen | xclip` style usage stays clean.
	fmt.Println(token)
	fmt.Fprint(os.Stderr, "add to central's probes.tokens and the probe's central.token, then restart both\n")
	return 0
}

// generateToken returns a new opaque agent token: agentTokenBytes of
// entropy from r, base64url-encoded without padding (43 chars, safe
// to paste into yaml and shell without quoting surprises).
func generateToken(r io.Reader) (string, error) {
	buf := make([]byte, agentTokenBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("token gen: read entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
