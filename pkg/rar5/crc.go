package rar5

// crc32Combine returns the CRC-32 (IEEE) of two byte sequences joined, given
// the CRC-32 of each and the length of the second. It lets the volumes of a
// file be checksummed independently and still yield the checksum of the
// whole file. This is the algorithm of zlib's crc32_combine.
func crc32Combine(crc1, crc2 uint32, len2 int64) uint32 {
	if len2 <= 0 {
		return crc1
	}

	var even, odd [32]uint32 // operators for 2^n zero bits

	// operator for one zero bit
	odd[0] = 0xEDB88320 // reflected CRC-32 polynomial
	for n, row := 1, uint32(1); n < 32; n, row = n+1, row<<1 {
		odd[n] = row
	}
	gf2Square(&even, &odd) // two zero bits
	gf2Square(&odd, &even) // four zero bits

	// apply len2 zero bytes to crc1, squaring the operator for each bit of len2
	for {
		gf2Square(&even, &odd)
		if len2&1 != 0 {
			crc1 = gf2Times(&even, crc1)
		}
		if len2 >>= 1; len2 == 0 {
			break
		}

		gf2Square(&odd, &even)
		if len2&1 != 0 {
			crc1 = gf2Times(&odd, crc1)
		}
		if len2 >>= 1; len2 == 0 {
			break
		}
	}

	return crc1 ^ crc2
}

func gf2Times(mat *[32]uint32, vec uint32) uint32 {
	var sum uint32
	for i := 0; vec != 0; i, vec = i+1, vec>>1 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
	}
	return sum
}

func gf2Square(square, mat *[32]uint32) {
	for n := range mat {
		square[n] = gf2Times(mat, mat[n])
	}
}
