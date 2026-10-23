package rsec16

import (
	"math/bits"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// additiveBasis uses a Cantor coordinate basis within the existing PAR2
// field. points/indices map between transform coordinates and field elements.
// s_i vanishes on the span of the first i coordinate basis elements.
type additiveBasis struct {
	points                           [1 << 16]gf2p16.T
	indices                          [1 << 16]uint16
	linear                           [16][]gf2p16.T
	denominator, inverse, derivative [16]gf2p16.T
}

func newAdditiveBasis() *additiveBasis {
	b := new(additiveBasis)
	// The Artin-Schreier map L(x)=x²+x is nilpotent of index 16.
	// Choose beta_15 with trace one, then L(beta_i)=beta_(i-1).
	// This gives a Cantor basis with s_i(beta_i)=s_i'(0)=1:
	// conversion and differentiation require XORs rather than field products.
	var coordinates [16]gf2p16.T
	for bit := 0; bit < 16; bit++ {
		root := gf2p16.T(1 << bit)
		x := root
		var trace gf2p16.T
		for j := 0; j < 16; j++ {
			trace ^= x
			x = x.Times(x)
		}
		if trace == 1 {
			coordinates[15] = root
			break
		}
	}
	for i := 14; i >= 0; i-- {
		x := coordinates[i+1]
		coordinates[i] = x.Times(x) ^ x
	}
	if coordinates[0] != 1 {
		panic("invalid PAR2 Cantor basis")
	}
	for i := 1; i < 1<<16; i++ {
		bit := bits.TrailingZeros(uint(i))
		b.points[i] = b.points[i^(1<<bit)] ^ coordinates[bit]
		b.indices[b.points[i]] = uint16(i)
	}
	p := []gf2p16.T{1} // s_0(x)=x; entries are coefficients of x^(2^j).
	for i := range b.linear {
		b.linear[i] = p
		d := evalLinear(p, coordinates[i])
		b.denominator[i], b.inverse[i] = d, d.Inverse()
		b.derivative[i] = p[0].Div(d)
		next := make([]gf2p16.T, len(p)+1)
		for j, c := range p {
			next[j+1] ^= c.Times(c)
			next[j] ^= d.Times(c)
		}
		p = next
	}
	return b
}

func evalLinear(p []gf2p16.T, x gf2p16.T) gf2p16.T {
	var y gf2p16.T
	for _, c := range p {
		y ^= c.Times(x)
		x = x.Times(x)
	}
	return y
}

func additiveLog(n int) int {
	if n < 1 || n > 1<<16 || n&(n-1) != 0 {
		panic("invalid additive transform length")
	}
	return bits.Len(uint(n)) - 1
}

func (b *additiveBasis) toNovel(v []gf2p16.T) {
	for i := additiveLog(len(v)) - 1; i >= 0; i-- {
		half := 1 << i
		for base := 0; base < len(v); base += 2 * half {
			for r := half - 1; r >= 0; r-- {
				d := base + half + r
				factor := v[d]
				for j := 0; j < i; j++ {
					v[base+r+(1<<j)] ^= factor.Times(b.linear[i][j])
				}
				v[d] = factor.Times(b.denominator[i])
			}
		}
	}
}
func (b *additiveBasis) fromNovel(v []gf2p16.T) {
	for i := 0; i < additiveLog(len(v)); i++ {
		half := 1 << i
		for base := 0; base < len(v); base += 2 * half {
			for r := 0; r < half; r++ {
				d := base + half + r
				factor := v[d].Times(b.inverse[i])
				v[d] = factor
				for j := 0; j < i; j++ {
					v[base+r+(1<<j)] ^= factor.Times(b.linear[i][j])
				}
			}
		}
	}
}
func (b *additiveBasis) fft(v []gf2p16.T, inverse bool) {
	levels := additiveLog(len(v))
	for step := 0; step < levels; step++ {
		i := levels - 1 - step
		if inverse {
			i = step
		}
		half := 1 << i
		for base := 0; base < len(v); base += 2 * half {
			c := evalLinear(b.linear[i], b.points[base]).Times(b.inverse[i])
			for r := 0; r < half; r++ {
				l, h := base+r, base+half+r
				if inverse {
					v[h] ^= v[l]
					v[l] ^= c.Times(v[h])
				} else {
					v[l] ^= c.Times(v[h])
					v[h] ^= v[l]
				}
			}
		}
	}
}
func (b *additiveBasis) differentiate(v []gf2p16.T) {
	additiveLog(len(v))
	for j, c := range v {
		for mask := j; mask != 0; mask &= mask - 1 {
			i := bits.TrailingZeros(uint(mask))
			v[j^(1<<i)] ^= c.Times(b.derivative[i])
		}
		v[j] = 0
	}
}
func (b *additiveBasis) product(a, c []gf2p16.T) []gf2p16.T {
	if len(a) == 0 || len(c) == 0 {
		return nil
	}
	count := len(a) + len(c) - 1
	if count <= 64 {
		out := make([]gf2p16.T, count)
		for i, x := range a {
			for j, y := range c {
				out[i+j] ^= x.Times(y)
			}
		}
		return out
	}
	n := 1 << bits.Len(uint(count-1))
	additiveLog(n)
	x, y := make([]gf2p16.T, n), make([]gf2p16.T, n)
	copy(x, a)
	copy(y, c)
	b.toNovel(x)
	b.toNovel(y)
	b.fft(x, false)
	b.fft(y, false)
	for i := range x {
		x[i] = x[i].Times(y[i])
	}
	b.fft(x, true)
	b.fromNovel(x)
	return x[:count]
}

func (b *additiveBasis) locator(points []gf2p16.T) []gf2p16.T {
	if len(points) == 0 {
		return []gf2p16.T{1}
	}
	if len(points) == 1 {
		return []gf2p16.T{points[0], 1}
	}
	mid := len(points) / 2
	return b.product(b.locator(points[:mid]), b.locator(points[mid:]))
}
