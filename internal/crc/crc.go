// Package crc centralises strata's checksum discipline.
//
// Every durable structure in the engine -- log records, data blocks, the
// manifest -- is checksummed with CRC32-Castagnoli and stored MASKED.
//
// Masking exists because CRC32 is linear. Feeding a buffer that already ends
// in its own CRC back through CRC32 produces a fixed, content-independent
// value, which means a naive nested checksum detects almost nothing. Rotating
// and offsetting the checksum before storing it breaks that linearity, so a
// checksum computed over already-checksummed bytes is still a real check.
//
// Castagnoli rather than IEEE because modern x86 and ARM implement it as a
// single instruction, which matters when every block write pays for it.
package crc

import "hash/crc32"

var table = crc32.MakeTable(crc32.Castagnoli)

const maskDelta uint32 = 0xa282ead8

// Value computes the unmasked CRC of the concatenation of the given chunks.
func Value(chunks ...[]byte) uint32 {
	var c uint32
	for _, b := range chunks {
		c = crc32.Update(c, table, b)
	}
	return c
}

// Mask transforms a raw CRC into the form stored on disk.
func Mask(c uint32) uint32 { return (c>>15 | c<<17) + maskDelta }

// Unmask reverses Mask.
func Unmask(m uint32) uint32 { rot := m - maskDelta; return rot>>17 | rot<<15 }

// Masked is shorthand for Mask(Value(chunks...)).
func Masked(chunks ...[]byte) uint32 { return Mask(Value(chunks...)) }
