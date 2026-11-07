package vector

import (
	"math"
	"testing"
)

// Cosine normalizes defensively (doctor feeds it raw embedder output) and returns 0, never
// NaN, for a length mismatch or a zero vector.
func TestCosine(t *testing.T) {
	for _, c := range []struct {
		a, b []float32
		want float64
	}{
		{[]float32{3, 0}, []float32{5, 0}, 1},
		{[]float32{1, 1}, []float32{-2, -2}, -1},
		{[]float32{1, 0}, []float32{0, 7}, 0},
		{[]float32{1, 0}, []float32{1, 0, 0}, 0},
		{[]float32{0, 0}, []float32{1, 0}, 0},
	} {
		if got := Cosine(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Cosine(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
