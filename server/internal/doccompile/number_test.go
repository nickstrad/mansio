package doccompile

import (
	"math"
	"testing"
)

// TestStringToNumber pins JavaScript's Number(text) where Go's ParseFloat disagrees.
func TestStringToNumber(t *testing.T) {
	for text, want := range map[string]float64{
		"": 0, "  12 ": 12, "1e3": 1000, ".5": 0.5, "5.": 5, "-0x10": math.NaN(), "0x10": 16, "0b101": 5, "0o17": 15,
		"Infinity": math.Inf(1), "-Infinity": math.Inf(-1), "inf": math.NaN(), "NaN": math.NaN(), "1_0": math.NaN(),
		"0x1p-2": math.NaN(), "1e400": math.Inf(1), "12abc": math.NaN(),
	} {
		got := stringToNumber(text)
		if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
			t.Errorf("Number(%q) = %v, want %v", text, got, want)
		}
	}
}
