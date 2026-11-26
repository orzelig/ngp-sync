package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The published "HELLO WORLD" 1-M example: its 16 data codewords and the 10
// Reed–Solomon EC codewords they must produce.
func TestRSKnownVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, rsDivisor(10)); !bytes.Equal(got, want) {
		t.Fatalf("EC codewords = %v, want %v", got, want)
	}
}

// Byte-mode data codewords: mode 0100, an 8-bit count, the bytes, a 4-bit
// terminator, then 0xEC/0x11 pad bytes up to version 1-M's 16 codewords.
func TestDataCodewordsPadding(t *testing.T) {
	got := qrDataCodewords("A", 1)
	// 0100 00000001 01000001 0000 -> 0x40 0x14 0x10, then pads.
	want := []byte{0x40, 0x14, 0x10, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC, 0x11, 0xEC}
	if !bytes.Equal(got, want) {
		t.Fatalf("data codewords = % x, want % x", got, want)
	}
}

// ISO/IEC 18004's format information table for level M, masks 0 to 7.
var formatM = []string{
	"101010000010010",
	"101000100100101",
	"101111001111100",
	"101101101001011",
	"100010111111001",
	"100000011001110",
	"100111110010111",
	"100101010100000",
}

func TestFormatBitsKnownVectors(t *testing.T) {
	for mask, want := range formatM {
		if got := fmt.Sprintf("%015b", qrFormatBits(mask)); got != want {
			t.Errorf("format bits for M, mask %d = %s, want %s", mask, got, want)
		}
	}
}

func TestStructureEveryVersion(t *testing.T) {
	// The longest input each version holds, so each version is reached.
	for v := 1; v <= qrMaxVersion; v++ {
		vi := qrVersionsM[v]
		text := strings.Repeat("x", vi.blocks*vi.dataPerBlock-2)
		m, err := qrEncode(text)
		if err != nil {
			t.Fatalf("v%d: %v", v, err)
		}
		size := 17 + 4*v
		if len(m) != size || len(m[0]) != size {
			t.Fatalf("v%d: size %dx%d, want %d", v, len(m), len(m[0]), size)
		}
		for _, c := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
			for dy := 0; dy < 7; dy++ {
				for dx := 0; dx < 7; dx++ {
					ring := max(qrAbs(dx-3), qrAbs(dy-3))
					if want := ring != 2; m[c[1]+dy][c[0]+dx] != want {
						t.Fatalf("v%d: finder at %v wrong at (%d,%d)", v, c, dx, dy)
					}
				}
			}
		}
		for i := 8; i < size-8; i++ {
			if m[6][i] != (i%2 == 0) || m[i][6] != (i%2 == 0) {
				t.Fatalf("v%d: timing pattern wrong at %d", v, i)
			}
		}
		if !m[size-8][8] {
			t.Fatalf("v%d: dark module missing", v)
		}
		if got, err := qrDecodeForTest(m); err != nil || got != text {
			t.Fatalf("v%d: decoded %q, %v", v, got, err)
		}
	}
}

func TestTooLong(t *testing.T) {
	if _, err := qrEncode(strings.Repeat("x", 107)); err == nil {
		t.Fatal("107 bytes encoded; want an error")
	}
	if _, err := qrEncode(strings.Repeat("x", 106)); err != nil {
		t.Fatalf("106 bytes: %v", err)
	}
}

// Every mask, the production payload's shape, and a two-block input (63
// bytes: version 5) read back to the same text with every block's
// Reed–Solomon syndromes zero.
func TestRoundTrip(t *testing.T) {
	inputs := []string{
		"https://id.ngp.one/pair?code=0123abcd",
		"http://localhost:8300/pair?code=deadbeef",
		strings.Repeat("ab", 31) + "c",
		"",
	}
	for _, in := range inputs {
		for mask := 0; mask < 8; mask++ {
			m, err := qrEncodeMask(in, mask)
			if err != nil {
				t.Fatalf("%q mask %d: %v", in, mask, err)
			}
			got, err := qrDecodeForTest(m)
			if err != nil || got != in {
				t.Fatalf("%q mask %d: decoded %q, %v", in, mask, got, err)
			}
		}
	}
	m, _ := qrEncode(inputs[0])
	if len(m) != 29 {
		t.Fatalf("production payload is %d modules wide, want 29 (version 3)", len(m))
	}
}

func TestSVG(t *testing.T) {
	m, _ := qrEncode("https://id.ngp.one/pair?code=0123abcd")
	s := string(qrSVG(m))
	if !strings.HasPrefix(s, "<svg") || !strings.Contains(s, `viewBox="0 0 37 37"`) || !strings.Contains(s, `fill="#fff"`) {
		t.Fatalf("unexpected svg: %.200s", s)
	}
	dark := 0
	for _, row := range m {
		for _, d := range row {
			if d {
				dark++
			}
		}
	}
	if n := strings.Count(s, "h1v1h-1z"); n != dark {
		t.Fatalf("svg draws %d modules, matrix has %d dark", n, dark)
	}
}

// qrDecodeForTest reads a symbol back, written apart from the encoder: its own
// reserved-area map, zigzag walk, mask formulas, GF arithmetic and block
// layout (from the ISO table), checking the format information and every
// block's Reed–Solomon syndromes before parsing the byte-mode segment.
func qrDecodeForTest(m [][]bool) (string, error) {
	size := len(m)
	version := (size - 17) / 4
	layout := map[int][3]int{ // blocks, data per block, EC per block (level M)
		1: {1, 16, 10}, 2: {1, 28, 16}, 3: {1, 44, 26},
		4: {2, 32, 18}, 5: {2, 43, 24}, 6: {4, 27, 16},
	}
	lay, ok := layout[version]
	if !ok || 17+4*version != size {
		return "", fmt.Errorf("size %d is not a version 1-6 symbol", size)
	}

	// Format information, first copy, bit 14 first.
	var fmtBits strings.Builder
	coords := [][2]int{ // (x, y) holding bits 14 down to 0
		{0, 8}, {1, 8}, {2, 8}, {3, 8}, {4, 8}, {5, 8}, {7, 8}, {8, 8},
		{8, 7}, {8, 5}, {8, 4}, {8, 3}, {8, 2}, {8, 1}, {8, 0},
	}
	for _, c := range coords {
		if m[c[1]][c[0]] {
			fmtBits.WriteByte('1')
		} else {
			fmtBits.WriteByte('0')
		}
	}
	mask := -1
	for i, f := range formatM {
		if f == fmtBits.String() {
			mask = i
		}
	}
	if mask < 0 {
		return "", fmt.Errorf("format bits %s are not level M", fmtBits.String())
	}
	// Second copy: bits 0-7 along row 8 from the right, 8-14 down column 8.
	var second strings.Builder
	for i := 14; i >= 0; i-- {
		var d bool
		if i < 8 {
			d = m[8][size-1-i]
		} else {
			d = m[size-15+i][8]
		}
		if d {
			second.WriteByte('1')
		} else {
			second.WriteByte('0')
		}
	}
	if second.String() != fmtBits.String() {
		return "", fmt.Errorf("format copies differ: %s, %s", fmtBits.String(), second.String())
	}

	reserved := func(x, y int) bool {
		switch {
		case x <= 8 && y <= 8, x >= size-8 && y <= 8, x <= 8 && y >= size-8:
			return true
		case x == 6 || y == 6:
			return true
		case version >= 2 && x >= size-9 && x <= size-5 && y >= size-9 && y <= size-5:
			return true
		}
		return false
	}
	masks := []func(x, y int) bool{
		func(x, y int) bool { return (y+x)%2 == 0 },
		func(x, y int) bool { return y%2 == 0 },
		func(x, y int) bool { return x%3 == 0 },
		func(x, y int) bool { return (y+x)%3 == 0 },
		func(x, y int) bool { return (y/2+x/3)%2 == 0 },
		func(x, y int) bool { return (y*x)%2+(y*x)%3 == 0 },
		func(x, y int) bool { return ((y*x)%2+(y*x)%3)%2 == 0 },
		func(x, y int) bool { return ((y+x)%2+(y*x)%3)%2 == 0 },
	}

	var bits []bool
	up := true
	for col := size - 1; col > 0; col -= 2 {
		if col == 6 {
			col--
		}
		for k := 0; k < size; k++ {
			y := k
			if up {
				y = size - 1 - k
			}
			for _, x := range []int{col, col - 1} {
				if !reserved(x, y) {
					bits = append(bits, m[y][x] != masks[mask](x, y))
				}
			}
		}
		up = !up
	}
	total := lay[0] * (lay[1] + lay[2])
	if len(bits) < total*8 {
		return "", fmt.Errorf("only %d data bits", len(bits))
	}
	cw := make([]byte, total)
	for i := range cw {
		for j := 0; j < 8; j++ {
			cw[i] <<= 1
			if bits[i*8+j] {
				cw[i] |= 1
			}
		}
	}

	mul := func(a, b byte) byte {
		var p byte
		for b > 0 {
			if b&1 == 1 {
				p ^= a
			}
			carry := a & 0x80
			a <<= 1
			if carry != 0 {
				a ^= 0x1D
			}
			b >>= 1
		}
		return p
	}
	nb, nd, ne := lay[0], lay[1], lay[2]
	var data []byte
	for b := 0; b < nb; b++ {
		block := make([]byte, 0, nd+ne)
		for i := 0; i < nd; i++ {
			block = append(block, cw[i*nb+b])
		}
		for i := 0; i < ne; i++ {
			block = append(block, cw[nd*nb+i*nb+b])
		}
		alpha := byte(1) // α^i
		for i := 0; i < ne; i++ {
			var s byte
			for _, c := range block {
				s = mul(s, alpha) ^ c
			}
			if s != 0 {
				return "", fmt.Errorf("block %d: syndrome %d is %d", b, i, s)
			}
			alpha = mul(alpha, 2)
		}
		data = append(data, block[:nd]...)
	}

	if data[0]>>4 != 0x4 {
		return "", fmt.Errorf("mode %x is not byte mode", data[0]>>4)
	}
	n := int(data[0]&0x0F)<<4 | int(data[1]>>4)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = data[1+i]<<4 | data[2+i]>>4
	}
	return string(out), nil
}
