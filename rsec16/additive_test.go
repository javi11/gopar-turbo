package rsec16

import (
	"github.com/javi11/gopar-turbo/gf2p16"
	"github.com/stretchr/testify/require"
	"math/rand"
	"testing"
)

func evalPolynomial(p []gf2p16.T, x gf2p16.T) gf2p16.T {
	var y gf2p16.T
	for i := len(p) - 1; i >= 0; i-- {
		y = y.Times(x) ^ p[i]
	}
	return y
}

func TestAdditiveEvaluationAndInverse(t *testing.T) {
	b := newAdditiveBasis()
	rng := rand.New(rand.NewSource(89))
	for _, n := range []int{2, 4, 8, 32, 256, 1024} {
		p := make([]gf2p16.T, n)
		for i := range p {
			p[i] = gf2p16.T(rng.Uint32())
		}
		values := append([]gf2p16.T(nil), p...)
		b.toNovel(values)
		b.fft(values, false)
		for x := range values {
			require.Equal(t, evalPolynomial(p, b.points[x]), values[x], "size %d point %d", n, x)
		}
		b.fft(values, true)
		b.fromNovel(values)
		require.Equal(t, p, values)
	}
}
func TestAdditiveDerivative(t *testing.T) {
	b := newAdditiveBasis()
	p := make([]gf2p16.T, 256)
	for i := range p {
		p[i] = gf2p16.T(i*73 + 7)
	}
	want := make([]gf2p16.T, len(p))
	for i := 1; i < len(p); i += 2 {
		want[i-1] = p[i]
	}
	b.toNovel(p)
	b.differentiate(p)
	b.fromNovel(p)
	require.Equal(t, want, p)
}
func TestAdditiveProduct(t *testing.T) {
	b := newAdditiveBasis()
	a, c := make([]gf2p16.T, 137), make([]gf2p16.T, 93)
	for i := range a {
		a[i] = gf2p16.T(i*7 + 1)
	}
	for i := range c {
		c[i] = gf2p16.T(i*3 + 2)
	}
	want := make([]gf2p16.T, len(a)+len(c)-1)
	for i, x := range a {
		for j, y := range c {
			want[i+j] ^= x.Times(y)
		}
	}
	require.Equal(t, want, b.product(a, c))
}
func TestAdditiveCauchyIdentity(t *testing.T) {
	b := newAdditiveBasis()
	values := make([]gf2p16.T, 1<<16)
	points := []int{0, 3, 127, 32768, 65535}
	weights := []gf2p16.T{5, 7, 11, 13, 17}
	for i, x := range points {
		values[b.indices[x]] = weights[i]
	}
	b.fft(values, true)
	b.differentiate(values)
	b.fft(values, false)
	for _, x := range []int{0, 1, 2, 3, 128, 512, 32768, 65534, 65535} {
		var want gf2p16.T
		for j, y := range points {
			if x != y {
				want ^= weights[j].Div(gf2p16.T(x ^ y))
			}
		}
		require.Equal(t, want, values[b.indices[x]], "point %d", x)
	}
}

func TestAdditiveCantorNormalization(t *testing.T) {
	b := newAdditiveBasis()
	for i := 0; i < 16; i++ {
		require.Equal(t, gf2p16.T(1), b.denominator[i], "Cantor subspace %d", i)
		require.Equal(t, gf2p16.T(1), b.derivative[i])
		for _, c := range b.linear[i] {
			require.True(t, c == 0 || c == 1)
		}
	}
}
