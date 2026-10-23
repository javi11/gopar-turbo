package rsec16

import (
	"sync"

	"github.com/javi11/gopar-turbo/gf2p16"
)

var par2AdditiveBasis = sync.OnceValue(newAdditiveBasis)

func consecutiveStart(exponents []int) (int, bool) {
	if len(exponents) == 0 {
		return 0, false
	}
	first := exponents[0]
	if first < 0 || first+len(exponents)-1 > 65535 {
		return 0, false
	}
	for i, e := range exponents {
		if e != first+i {
			return 0, false
		}
	}
	return first, true
}

// locatorValues computes P and its derivative on the full additive field.
// Scalar preprocessing is shared by matrix planning and vector transforms.
func locatorValues(missing []int) (polynomial, values, derivative []gf2p16.T) {
	b := par2AdditiveBasis()
	points := make([]gf2p16.T, len(missing))
	for i, row := range missing {
		points[i] = generators[row]
	}
	polynomial = b.locator(points)
	values = make([]gf2p16.T, 1<<16)
	copy(values, polynomial)
	b.toNovel(values)
	derivative = append([]gf2p16.T(nil), values...)
	b.differentiate(derivative)
	b.fft(values, false)
	b.fft(derivative, false)
	// Consumers use field elements as keys; transform buffers use Cantor indices.
	fieldValues, fieldDerivative := make([]gf2p16.T, 1<<16), make([]gf2p16.T, 1<<16)
	for i, x := range b.points {
		fieldValues[x] = values[i]
		fieldDerivative[x] = derivative[i]
	}
	values, derivative = fieldValues, fieldDerivative
	return
}

// par2LagrangeMatrix solves a transposed Vandermonde system with consecutive
// recovery exponents using its Lagrange basis, in O(k²+kn), not O(k³+k²n).
// Column order remains survivors followed by the chosen recovery blocks.
func par2LagrangeMatrix(n int, available, missing, exponents []int) (gf2p16.Matrix, bool) {
	start, ok := consecutiveStart(exponents)
	if !ok || len(missing) != len(exponents) {
		return gf2p16.Matrix{}, false
	}
	k := len(missing)
	p, values, derivative := locatorValues(missing)
	weights := make([]gf2p16.T, len(available))
	for j, row := range available {
		y := generators[row]
		weights[j] = values[y].Times(y.Pow(uint32(start)))
	}
	result := make([]gf2p16.T, k*n)
	quotient := make([]gf2p16.T, k)
	for i, row := range missing {
		x := generators[row]
		scale := derivative[x].Times(x.Pow(uint32(start))).Inverse()
		dst := result[i*n : (i+1)*n]
		for j, other := range available {
			dst[j] = weights[j].Div(generators[other] ^ x).Times(scale)
		}
		quotient[k-1] = p[k]
		for j := k - 2; j >= 0; j-- {
			quotient[j] = p[j+1] ^ x.Times(quotient[j+1])
		}
		for j, c := range quotient {
			dst[len(available)+j] = c.Times(scale)
		}
	}
	return gf2p16.NewMatrixFromSlice(k, n, result), true
}
