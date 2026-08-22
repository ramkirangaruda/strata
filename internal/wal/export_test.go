package wal

import "github.com/ramkirangaruda/strata/internal/crc"

func crcMaskForTest(v uint32) uint32 { return crc.Mask(v) }
