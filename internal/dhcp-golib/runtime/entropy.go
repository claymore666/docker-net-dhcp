package runtime

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
)

// Entropy is the real source of the rnd value each Step consumes.
//
// crypto/rand seeds it; a splitmix64 generator produces the stream. Not
// math/rand: the transaction id derives from this, and an xid an attacker can
// predict is an xid it can forge (RFC 2131 section 4.1). Not crypto/rand per
// call either — the machine consumes a value on EVERY Step, and a syscall per
// timer fire buys nothing.
//
// A seed failure is fatal: a client whose xids can be guessed is worse than a
// client that did not start.
type Entropy struct {
	mu    sync.Mutex
	state uint64
}

// NewEntropy seeds from crypto/rand.
func NewEntropy() (*Entropy, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &Entropy{state: binary.BigEndian.Uint64(b[:])}, nil
}

// NewEntropySeeded returns a deterministic source. Exported rather than
// test-only because replaying a recorded journal is a production feature
// (requirement G6), not a test fixture.
func NewEntropySeeded(seed uint64) *Entropy { return &Entropy{state: seed} }

// Uint64 returns the next value.
func (e *Entropy) Uint64() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state += 0x9E3779B97F4A7C15
	z := e.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
