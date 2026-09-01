// Package ipx contains small helpers for working with IP ranges as sortable keys.
package ipx

import (
	"bytes"
	"net/netip"
)

// Key is the 16-byte big-endian form of an address. IPv4 addresses are stored
// in their 4-in-6 mapped form so that v4 and v6 ranges can live in one sorted
// list and still compare correctly.
type Key [16]byte

func KeyOf(a netip.Addr) Key { return Key(a.Unmap().As16()) }

func (k Key) Compare(o Key) int { return bytes.Compare(k[:], o[:]) }

func (k Key) LessOrEqual(o Key) bool { return k.Compare(o) <= 0 }

// Addr converts a key back to an address, unmapping 4-in-6 where applicable.
func (k Key) Addr() netip.Addr { return netip.AddrFrom16(k).Unmap() }

// Size returns the number of addresses in [start,end], saturating at ^uint64(0).
// It is used only to pick the most specific of several overlapping ranges.
func Size(start, end Key) uint64 {
	// Only the low 8 bytes matter for comparing range sizes at the scale we
	// care about; anything larger saturates and compares as "huge".
	for i := 0; i < 8; i++ {
		if start[i] != end[i] {
			return ^uint64(0)
		}
	}
	var s, e uint64
	for i := 8; i < 16; i++ {
		s = s<<8 | uint64(start[i])
		e = e<<8 | uint64(end[i])
	}
	if e < s {
		return 0
	}
	return e - s + 1
}
