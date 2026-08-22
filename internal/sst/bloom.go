package sst

import "encoding/binary"

// A bloom filter answers "is this key definitely absent?" without touching
// disk. That is the entire point: a point lookup that misses in a table should
// cost zero I/O, and in an LSM tree the overwhelming majority of lookups miss
// in the overwhelming majority of tables.
//
// The filter can produce false positives (it says "maybe present" for a key
// that is not there, costing one wasted block read) but never false negatives
// (it never says "absent" for a key that is there). A false negative would be
// data loss, so every design decision here is biased towards making that
// impossible: the filter is built from the same key set that is written, in
// the same pass, and is checksummed with the file.
//
// strata filters on USER keys, not internal keys. Internal keys embed a
// sequence number, so filtering on them would mean the filter could only
// answer "was this exact version written", which is never the question a
// lookup asks.

const (
	// bitsPerKey trades filter size against false-positive rate. At 10 bits
	// per key the rate is roughly 1%, which costs ~1.25 bytes of RAM per key
	// to avoid ~99% of pointless block reads. Raising it has sharply
	// diminishing returns; the curve is flat past about 16.
	bitsPerKey = 10

	// maxProbes caps k. Beyond ~30 probes the filter is slower than the disk
	// read it is trying to avoid.
	maxProbes = 30
)

// bloomHash is the 32-bit hash from LevelDB: a Murmur-like mixer chosen
// because it is fast and has good avalanche behaviour on short keys, which is
// what database keys overwhelmingly are.
func bloomHash(data []byte) uint32 {
	const (
		seed uint32 = 0xbc9f1d34
		m    uint32 = 0xc6a4a793
	)
	h := seed ^ uint32(len(data))*m
	for len(data) >= 4 {
		h += binary.LittleEndian.Uint32(data[:4])
		h *= m
		h ^= h >> 16
		data = data[4:]
	}
	switch len(data) {
	case 3:
		h += uint32(data[2]) << 16
		fallthrough
	case 2:
		h += uint32(data[1]) << 8
		fallthrough
	case 1:
		h += uint32(data[0])
		h *= m
		h ^= h >> 24
	}
	return h
}

// buildBloom returns a filter covering ukeys.
//
// Only one hash is computed per key. The k probe positions are derived from it
// by repeatedly adding a rotation of the same hash -- "double hashing". Using
// k independent hash functions gives no measurable improvement in
// false-positive rate but costs k times the CPU, so nobody does it.
func buildBloom(ukeys [][]byte) []byte {
	// 0.69 ~= ln 2, which minimises the false-positive rate for a given
	// bits-per-key budget. Computed at run time rather than as a constant
	// expression so that bitsPerKey stays a single tunable number.
	bpk := float64(bitsPerKey)
	probes := int(bpk * 0.69)
	if probes < 1 {
		probes = 1
	}
	if probes > maxProbes {
		probes = maxProbes
	}

	bits := len(ukeys) * bitsPerKey
	if bits < 64 {
		// A tiny filter has a false-positive rate near 1 and is worse than
		// useless, so enforce a floor.
		bits = 64
	}
	nbytes := (bits + 7) / 8
	bits = nbytes * 8

	// One trailing byte records k, so a reader does not have to be compiled
	// with the same constants the writer used. Formats outlive binaries.
	filter := make([]byte, nbytes+1)
	filter[nbytes] = byte(probes)

	for _, k := range ukeys {
		h := bloomHash(k)
		delta := h>>17 | h<<15
		for j := 0; j < probes; j++ {
			pos := int(h % uint32(bits))
			filter[pos/8] |= 1 << (pos % 8)
			h += delta
		}
	}
	return filter
}

// bloomMayContain reports whether ukey might be present.
//
// It returns true for any filter it cannot interpret. Failing "open" is
// mandatory here: an unreadable filter must degrade into a wasted block read,
// never into a reported miss for a key that exists.
func bloomMayContain(filter, ukey []byte) bool {
	if len(filter) < 2 {
		return true
	}
	nbytes := len(filter) - 1
	probes := int(filter[nbytes])
	if probes > maxProbes {
		return true // reserved encoding from a future format version
	}
	bits := uint32(nbytes * 8)

	h := bloomHash(ukey)
	delta := h>>17 | h<<15
	for j := 0; j < probes; j++ {
		pos := h % bits
		if filter[pos/8]&(1<<(pos%8)) == 0 {
			return false
		}
		h += delta
	}
	return true
}
