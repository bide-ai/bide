package eval

// Test-only accessors that expose the unexported statistical helpers so the external eval_test
// package can check them against known reference values.

func TwoProportionPForTest(x1, n1, x2, n2 int) float64 { return twoProportionP(x1, n1, x2, n2) }
func FishersExactForTest(a, b, c, d int) float64       { return fishersExact(a, b, c, d) }
func BHAdjustForTest(p []float64) []float64            { return bhAdjust(p) }
func ProbitForTest(p float64) float64                  { return probit(p) }
