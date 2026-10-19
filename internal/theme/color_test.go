package theme

import (
	"math"
	"testing"
)

func mustColor(t *testing.T, s string) rgb {
	t.Helper()
	c, err := parseColor(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestContrastWCAG: the ratio against the published WCAG 2.x values. #767676 on white is the
// lightest grey that passes 4.5:1 (4.54); #777777 does not (4.48).
func TestContrastWCAG(t *testing.T) {
	white := mustColor(t, "#fff")
	for _, tc := range []struct {
		fg   string
		want float64
	}{{"#000000", 21}, {"#ffffff", 1}, {"#767676", 4.54}, {"#777777", 4.48}, {"#595959", 7.0}} {
		got := contrast(mustColor(t, tc.fg), white)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("contrast(%s, white) = %.3f, want %.2f", tc.fg, got, tc.want)
		}
	}
	// It is symmetric and not rounded: 4.4999 must not pass as 4.5.
	if a, b := contrast(mustColor(t, "#777777"), white), contrast(white, mustColor(t, "#777777")); a != b {
		t.Errorf("contrast is not symmetric: %v and %v", a, b)
	}
	if contrast(mustColor(t, "#777777"), white) >= textContrast {
		t.Error("#777777 on white passes 4.5:1; it is 4.48")
	}
	if contrast(mustColor(t, "#767676"), white) < textContrast {
		t.Error("#767676 on white fails 4.5:1; it is 4.54")
	}
}

// TestDeltaE2000Sharma checks CIEDE2000 against the 34 test pairs of Sharma, Wu and Dalal (2005),
// "The CIEDE2000 colour-difference formula: implementation notes, supplementary test data".
func TestDeltaE2000Sharma(t *testing.T) {
	data := [][7]float64{
		{50, 2.6772, -79.7751, 50, 0, -82.7485, 2.0425},
		{50, 3.1571, -77.2803, 50, 0, -82.7485, 2.8615},
		{50, 2.8361, -74.0200, 50, 0, -82.7485, 3.4412},
		{50, -1.3802, -84.2814, 50, 0, -82.7485, 1.0000},
		{50, -1.1848, -84.8006, 50, 0, -82.7485, 1.0000},
		{50, -0.9009, -85.5211, 50, 0, -82.7485, 1.0000},
		{50, 0, 0, 50, -1, 2, 2.3669},
		{50, -1, 2, 50, 0, 0, 2.3669},
		{50, 2.49, -0.001, 50, -2.49, 0.0009, 7.1792},
		{50, 2.49, -0.001, 50, -2.49, 0.0010, 7.1792},
		{50, 2.49, -0.001, 50, -2.49, 0.0011, 7.2195},
		{50, 2.49, -0.001, 50, -2.49, 0.0012, 7.2195},
		{50, -0.001, 2.49, 50, 0.0009, -2.49, 4.8045},
		{50, -0.001, 2.49, 50, 0.0010, -2.49, 4.8045},
		{50, -0.001, 2.49, 50, 0.0011, -2.49, 4.7461},
		{50, 2.5, 0, 50, 0, -2.5, 4.3065},
		{50, 2.5, 0, 73, 25, -18, 27.1492},
		{50, 2.5, 0, 61, -5, 29, 22.8977},
		{50, 2.5, 0, 56, -27, -3, 31.9030},
		{50, 2.5, 0, 58, 24, 15, 19.4535},
		{50, 2.5, 0, 50, 3.1736, 0.5854, 1.0000},
		{50, 2.5, 0, 50, 3.2972, 0, 1.0000},
		{50, 2.5, 0, 50, 1.8634, 0.5757, 1.0000},
		{50, 2.5, 0, 50, 3.2592, 0.3350, 1.0000},
		{60.2574, -34.0099, 36.2677, 60.4626, -34.1751, 39.4387, 1.2644},
		{63.0109, -31.0961, -5.8663, 62.8187, -29.7946, -4.0864, 1.2630},
		{61.2901, 3.7196, -5.3901, 61.4292, 2.2480, -4.9620, 1.8731},
		{35.0831, -44.1164, 3.7933, 35.0232, -40.0716, 1.5901, 1.8645},
		{22.7233, 20.0904, -46.6940, 23.0331, 14.9730, -42.5619, 2.0373},
		{36.4612, 47.8580, 18.3852, 36.2715, 50.5065, 21.2231, 1.4146},
		{90.8027, -2.0831, 1.4410, 91.1528, -1.6435, 0.0447, 1.4441},
		{90.9257, -0.5406, -0.9208, 88.6381, -0.8985, -0.7239, 1.5381},
		{6.7747, -0.2908, -2.4247, 5.8714, -0.0985, -2.2286, 0.6377},
		{2.0776, 0.0795, -1.1350, 0.9033, -0.0636, -0.5514, 0.9082},
	}
	for i, d := range data {
		got := deltaE2000(lab{d[0], d[1], d[2]}, lab{d[3], d[4], d[5]})
		if math.Abs(got-d[6]) > 5e-5 {
			t.Errorf("pair %d: deltaE2000 = %.4f, want %.4f", i+1, got, d[6])
		}
	}
}

// TestColorBlindSimulation: the matrices keep greys grey (each row sums to 1), and the classic
// confusions collapse: red and green are far apart for a typical eye and close under deuteranopia.
func TestColorBlindSimulation(t *testing.T) {
	for v := protanopia; v <= tritanopia; v++ {
		for i, row := range machado[v] {
			if sum := row[0] + row[1] + row[2]; math.Abs(sum-1) > 1e-3 {
				t.Errorf("%v row %d sums to %.4f, want 1", v, i, sum)
			}
		}
		g := mustColor(t, "#808080").simulate(v)
		if c := math.Hypot(g.a, g.b); c > 1 {
			t.Errorf("%v: a grey gains chroma %.2f", v, c)
		}
	}
	red, green := mustColor(t, "#cc0000"), mustColor(t, "#00aa00")
	if d := deltaE2000(red.lab(), green.lab()); d < 50 {
		t.Errorf("red and green are %.1f apart for a typical eye, want a large distance", d)
	}
	if d := deltaE2000(red.simulate(deuteranopia), green.simulate(deuteranopia)); d > 25 {
		t.Errorf("red and green are still %.1f apart under deuteranopia", d)
	}
}

func TestParseColor(t *testing.T) {
	for in, want := range map[string]string{"#fff": "#ffffff", "#ABC": "#aabbcc", "#1B52b0": "#1b52b0", "#000": "#000000"} {
		c, err := parseColor(in)
		if err != nil || c.String() != want {
			t.Errorf("parseColor(%q) = %v, %v; want %s", in, c, err, want)
		}
	}
	for _, in := range []string{"", "red", "#", "#12", "#1234", "#12345", "#1234567", "#12345678", "#ggg", "#-ff", "#+ff", "# fff", "#fff ", " #fff", "rgb(1,2,3)", "var(--x)", "url(x)", "#fff\n", "currentcolor", "Canvas"} {
		if _, err := parseColor(in); err == nil {
			t.Errorf("parseColor(%q) succeeded; want an error", in)
		}
	}
}
