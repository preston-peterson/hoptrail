package main

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestGenerateToken_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := generateToken(rand.Reader)
		if err != nil {
			t.Fatalf("generateToken: %v", err)
		}
		// 32 bytes → 43 chars of unpadded base64url.
		if len(tok) != 43 {
			t.Fatalf("token length = %d, want 43: %q", len(tok), tok)
		}
		// base64url alphabet only — no '+', '/', '=' that would need
		// quoting care in yaml or shell.
		if strings.ContainsAny(tok, "+/=") {
			t.Fatalf("token contains non-url-safe chars: %q", tok)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %q", tok)
		}
		seen[tok] = true
	}
}

func TestGenerateToken_EntropyFailureSurfaces(t *testing.T) {
	if _, err := generateToken(failingReader{}); err == nil {
		t.Fatal("want error from failing entropy source, got nil")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
