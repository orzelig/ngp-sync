package main

// A small, standard-library-only QR code encoder for the "Add a phone" page
// (ADR 0005). Deliberately narrow: byte mode, error-correction level M,
// versions 1 to 6 (up to 106 bytes), which covers <BASE_URL>/pair?code=<code>
// with room to spare. Layout, Reed–Solomon and masking follow ISO/IEC 18004.

import (
	"errors"
	"fmt"
	"html/template"
	"strings"
)

// qrVersionM describes one version at level M.
type qrVersionM struct {
	blocks       int // number of blocks (all the same size for versions 1-6 at M)
	dataPerBlock int // data codewords per block
	ecPerBlock   int // error-correction codewords per block
}

// qrVersionsM is indexed by version; entry 0 is unused.
var qrVersionsM = [...]qrVersionM{
	{},
	{1, 16, 10},
	{1, 28, 16},
	{1, 44, 26},
	{2, 32, 18},
	{2, 43, 24},
	{4, 27, 16},
}

const qrMaxVersion = len(qrVersionsM) - 1

var errQRTooLong = errors.New("qr: text too long for versions 1-6 at level M")

func qrSize(version int) int { return 17 + 4*version }

// qrEncode returns the module matrix for text (true = dark), without the quiet
// zone, using the mask with the lowest penalty score.
func qrEncode(text string) ([][]bool, error) {
	var best [][]bool
	bestPenalty := -1
	for mask := 0; mask < 8; mask++ {
		m, err := qrEncodeMask(text, mask)
		if err != nil {
			return nil, err
		}
		if p := qrPenalty(m); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
	}
	return best, nil
}

// qrEncodeMask is qrEncode with the mask pattern (0-7) forced.
func qrEncodeMask(text string, mask int) ([][]bool, error) {
	if mask < 0 || mask > 7 {
		return nil, fmt.Errorf("qr: mask %d out of range", mask)
	}
	version := 0
	for v := 1; v <= qrMaxVersion; v++ {
		vi := qrVersionsM[v]
		// 4 bits of mode + 8 bits of count, then 8 bits per byte.
		if len(text) <= vi.blocks*vi.dataPerBlock-2 {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, errQRTooLong
	}
	codewords := qrAddEC(qrDataCodewords(text, version), version)

	modules, isFunc := qrFunctionPatterns(version)
	qrPlaceData(modules, isFunc, codewords)
	qrApplyMask(modules, isFunc, mask)
	qrDrawFormat(modules, mask)
	return modules, nil
}

// qrDataCodewords builds the data codewords: byte mode indicator, 8-bit
// count, the bytes, a terminator, padding to a byte, then 0xEC/0x11 pad bytes.
func qrDataCodewords(text string, version int) []byte {
	vi := qrVersionsM[version]
	capBits := vi.blocks * vi.dataPerBlock * 8
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>i)&1 == 1)
		}
	}
	put(0x4, 4)
	put(len(text), 8)
	for i := 0; i < len(text); i++ {
		put(int(text[i]), 8)
	}
	for i := 0; i < 4 && len(bits) < capBits; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	out := make([]byte, 0, capBits/8)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			b <<= 1
			if bits[i+j] {
				b |= 1
			}
		}
		out = append(out, b)
	}
	for pad := byte(0xEC); len(out) < capBits/8; pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

// qrAddEC splits data into blocks, computes each block's EC codewords and
// returns the interleaved data codewords followed by the interleaved EC ones.
func qrAddEC(data []byte, version int) []byte {
	vi := qrVersionsM[version]
	div := rsDivisor(vi.ecPerBlock)
	blocks := make([][]byte, vi.blocks)
	ecs := make([][]byte, vi.blocks)
	for b := 0; b < vi.blocks; b++ {
		blocks[b] = data[b*vi.dataPerBlock : (b+1)*vi.dataPerBlock]
		ecs[b] = rsRemainder(blocks[b], div)
	}
	out := make([]byte, 0, vi.blocks*(vi.dataPerBlock+vi.ecPerBlock))
	for i := 0; i < vi.dataPerBlock; i++ {
		for b := range blocks {
			out = append(out, blocks[b][i])
		}
	}
	for i := 0; i < vi.ecPerBlock; i++ {
		for b := range ecs {
			out = append(out, ecs[b][i])
		}
	}
	return out
}

// gfMul multiplies in GF(2^8) with the QR polynomial x^8+x^4+x^3+x^2+1.
func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		hi := z & 0x80
		z <<= 1
		if hi != 0 {
			z ^= 0x1D
		}
		if (y>>i)&1 == 1 {
			z ^= x
		}
	}
	return z
}

// rsDivisor returns the coefficients (highest power first, leading 1 omitted)
// of the generator polynomial (x - α^0)(x - α^1)...(x - α^(degree-1)).
func rsDivisor(degree int) []byte {
	result := make([]byte, degree)
	result[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range result {
			result[j] = gfMul(result[j], root)
			if j+1 < len(result) {
				result[j] ^= result[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return result
}

// rsRemainder returns the EC codewords for data under the given divisor.
func rsRemainder(data, divisor []byte) []byte {
	result := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for i := range result {
			result[i] ^= gfMul(divisor[i], factor)
		}
	}
	return result
}

func qrNewMatrix(size int) [][]bool {
	m := make([][]bool, size)
	for i := range m {
		m[i] = make([]bool, size)
	}
	return m
}

// qrFunctionPatterns draws the finder, timing and alignment patterns and the
// dark module, reserves the format areas, and returns the matrix with a map of
// which modules are function modules. Indexing is m[y][x].
func qrFunctionPatterns(version int) (modules, isFunc [][]bool) {
	size := qrSize(version)
	modules, isFunc = qrNewMatrix(size), qrNewMatrix(size)
	set := func(x, y int, dark bool) {
		modules[y][x] = dark
		isFunc[y][x] = true
	}
	for i := 0; i < size; i++ {
		set(6, i, i%2 == 0)
		set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {size - 4, 3}, {3, size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x < 0 || y < 0 || x >= size || y >= size {
					continue
				}
				d := max(qrAbs(dx), qrAbs(dy))
				set(x, y, d != 2 && d != 4)
			}
		}
	}
	if version >= 2 {
		c := size - 7
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				set(c+dx, c+dy, max(qrAbs(dx), qrAbs(dy)) != 1)
			}
		}
	}
	qrDrawFormat(modules, 0) // reserve; redrawn with the real mask later
	for y := range isFunc {
		for x := range isFunc[y] {
			if qrIsFormatModule(size, x, y) {
				isFunc[y][x] = true
			}
		}
	}
	return modules, isFunc
}

func qrAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// qrIsFormatModule reports whether (x, y) holds format information or the
// dark module.
func qrIsFormatModule(size, x, y int) bool {
	switch {
	case x == 8 && (y <= 8 || y >= size-8):
		return true
	case y == 8 && (x <= 8 || x >= size-8):
		return true
	}
	return false
}

// qrFormatBits returns the 15-bit format information for level M and mask.
func qrFormatBits(mask int) int {
	data := 0<<3 | mask // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

// qrDrawFormat writes both copies of the format information and the dark
// module.
func qrDrawFormat(m [][]bool, mask int) {
	size := len(m)
	bits := qrFormatBits(mask)
	bit := func(i int) bool { return (bits>>i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		m[i][8] = bit(i)
	}
	m[7][8] = bit(6)
	m[8][8] = bit(7)
	m[8][7] = bit(8)
	for i := 9; i < 15; i++ {
		m[8][14-i] = bit(i)
	}
	for i := 0; i < 8; i++ {
		m[8][size-1-i] = bit(i)
	}
	for i := 8; i < 15; i++ {
		m[size-15+i][8] = bit(i)
	}
	m[size-8][8] = true // the dark module
}

// qrZigzag calls f for every non-function module in data placement order.
func qrZigzag(isFunc [][]bool, f func(x, y int)) {
	size := len(isFunc)
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		upward := (right+1)&2 == 0
		for vert := 0; vert < size; vert++ {
			y := vert
			if upward {
				y = size - 1 - vert
			}
			for j := 0; j < 2; j++ {
				if x := right - j; !isFunc[y][x] {
					f(x, y)
				}
			}
		}
	}
}

// qrPlaceData writes the codewords' bits along the zigzag; the remainder bits
// left over stay light.
func qrPlaceData(m, isFunc [][]bool, codewords []byte) {
	i := 0
	qrZigzag(isFunc, func(x, y int) {
		if i < len(codewords)*8 {
			m[y][x] = (codewords[i>>3]>>(7-i&7))&1 == 1
			i++
		}
	})
}

func qrMaskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func qrApplyMask(m, isFunc [][]bool, mask int) {
	for y := range m {
		for x := range m[y] {
			if !isFunc[y][x] && qrMaskBit(mask, x, y) {
				m[y][x] = !m[y][x]
			}
		}
	}
}

// qrPenalty scores a finished symbol by the four ISO/IEC 18004 rules; lower
// is better.
func qrPenalty(m [][]bool) int {
	size := len(m)
	at := func(x, y int, transpose bool) bool {
		if transpose {
			return m[x][y]
		}
		return m[y][x]
	}
	finder := []bool{true, false, true, true, true, false, true}
	p := 0
	for _, tr := range []bool{false, true} {
		for y := 0; y < size; y++ {
			// Rule 1: runs of five or more of one colour.
			run := 1
			for x := 1; x <= size; x++ {
				if x < size && at(x, y, tr) == at(x-1, y, tr) {
					run++
					continue
				}
				if run >= 5 {
					p += 3 + run - 5
				}
				run = 1
			}
			// Rule 3: 1:1:3:1:1 finder-like patterns with four light modules
			// on one side.
			for x := 0; x+7 <= size; x++ {
				match := true
				for k, want := range finder {
					if at(x+k, y, tr) != want {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				lightBefore, lightAfter := x >= 4, x+11 <= size
				for k := 1; k <= 4; k++ {
					if lightBefore && at(x-k, y, tr) {
						lightBefore = false
					}
					if lightAfter && at(x+6+k, y, tr) {
						lightAfter = false
					}
				}
				if lightBefore || lightAfter {
					p += 40
				}
			}
		}
	}
	// Rule 2: 2x2 blocks of one colour.
	dark := 0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if m[y][x] {
				dark++
			}
			if x+1 < size && y+1 < size {
				c := m[y][x]
				if m[y][x+1] == c && m[y+1][x] == c && m[y+1][x+1] == c {
					p += 3
				}
			}
		}
	}
	// Rule 4: 10 points per 5% the dark share is away from 50%.
	pct := dark * 100 / (size * size)
	p += qrAbs(pct-50) / 5 * 10
	return p
}

// qrSVG draws a module matrix as an inline black-on-white SVG with a
// four-module quiet zone. Built from booleans only: no text reaches the markup.
func qrSVG(m [][]bool) template.HTML {
	n := len(m) + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := range m {
		for x := range m[y] {
			if m[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String())
}
