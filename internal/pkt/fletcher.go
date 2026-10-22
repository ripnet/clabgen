package pkt

// Fletcher computes the ISO 8473 checksum (IS-IS LSPs, OSPF LSAs) of b with
// the checksum field at off (the two bytes there are treated as zero).
func Fletcher(b []byte, off int) uint16 {
	var c0, c1 int
	for i, v := range b {
		if i == off || i == off+1 {
			v = 0
		}
		c0 = (c0 + int(v)) % 255
		c1 = (c1 + c0) % 255
	}
	n := len(b) - off - 1 // bytes after the first checksum byte, minus one
	x := ((n*c0-c1)%255 + 255) % 255
	y := ((c1-(n+1)*c0)%255 + 255*2) % 255
	if x == 0 {
		x = 255
	}
	if y == 0 {
		y = 255
	}
	return uint16(x)<<8 | uint16(y)
}

// FletcherOK verifies a checksummed region.
func FletcherOK(b []byte) bool {
	var c0, c1 int
	for _, v := range b {
		c0 = (c0 + int(v)) % 255
		c1 = (c1 + c0) % 255
	}
	return c0 == 0 && c1 == 0
}
