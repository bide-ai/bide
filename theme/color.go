package theme

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/bide-ai/bide/internal/strictjson"
)

// rgb is an sRGB colour with 8 bits a channel, the only colour form a theme holds.
type rgb struct{ r, g, b uint8 }

// colorRE is the whole colour grammar: #rgb or #rrggbb, hex digits only. It is a pattern and not a
// strconv parse, which would accept a sign.
var colorRE = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// parseColor reads #rgb or #rrggbb.
func parseColor(s string) (rgb, error) {
	if !colorRE.MatchString(s) {
		return rgb{}, fmt.Errorf("%s is not a colour: want #rgb or #rrggbb", strictjson.Quote(s))
	}
	h := strings.ToLower(s[1:])
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	var c rgb
	var v [3]uint8
	for i := range v {
		hi, lo := unhex(h[2*i]), unhex(h[2*i+1])
		v[i] = hi<<4 | lo
	}
	c.r, c.g, c.b = v[0], v[1], v[2]
	return c, nil
}

func unhex(b byte) uint8 {
	if b >= 'a' {
		return b - 'a' + 10
	}
	return b - '0'
}

// String is the canonical spelling: #rrggbb in lower case.
func (c rgb) String() string { return fmt.Sprintf("#%02x%02x%02x", c.r, c.g, c.b) }

// linearize undoes the sRGB transfer function for a channel in 0..1.
func linearize(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func (c rgb) linear() [3]float64 {
	return [3]float64{linearize(float64(c.r) / 255), linearize(float64(c.g) / 255), linearize(float64(c.b) / 255)}
}

// luminance is the WCAG 2.x relative luminance.
func (c rgb) luminance() float64 {
	l := c.linear()
	return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
}

// contrast is the WCAG 2.x contrast ratio of two colours, unrounded: 4.49 is not 4.5.
func contrast(a, b rgb) float64 {
	la, lb := a.luminance(), b.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// lab is a colour in CIELAB under D65.
type lab struct{ l, a, b float64 }

func labOfLinear(r, g, b float64) lab {
	x := 0.4124564*r + 0.3575761*g + 0.1804375*b
	y := 0.2126729*r + 0.7151522*g + 0.0721750*b
	z := 0.0193339*r + 0.1191920*g + 0.9503041*b
	f := func(t float64) float64 {
		if t > 216.0/24389 {
			return math.Cbrt(t)
		}
		return (24389.0/27*t + 16) / 116
	}
	fx, fy, fz := f(x/0.95047), f(y), f(z/1.08883)
	return lab{116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)}
}

func (c rgb) lab() lab {
	l := c.linear()
	return labOfLinear(l[0], l[1], l[2])
}

// lch returns the chroma and the hue angle in degrees (0 to 360) of a colour in CIELAB.
func (c rgb) lch() (chroma, hue float64) {
	l := c.lab()
	chroma = math.Hypot(l.a, l.b)
	hue = math.Atan2(l.b, l.a) * 180 / math.Pi
	if hue < 0 {
		hue += 360
	}
	return chroma, hue
}

// deltaE2000 is the CIEDE2000 colour difference of two Lab colours (kL = kC = kH = 1).
func deltaE2000(x, y lab) float64 {
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	deg := func(r float64) float64 { return r * 180 / math.Pi }
	c1, c2 := math.Hypot(x.a, x.b), math.Hypot(y.a, y.b)
	cb := (c1 + c2) / 2
	g := 0.5 * (1 - math.Sqrt(math.Pow(cb, 7)/(math.Pow(cb, 7)+math.Pow(25, 7))))
	a1, a2 := (1+g)*x.a, (1+g)*y.a
	c1p, c2p := math.Hypot(a1, x.b), math.Hypot(a2, y.b)
	h := func(b, a float64) float64 {
		if b == 0 && a == 0 {
			return 0
		}
		v := deg(math.Atan2(b, a))
		if v < 0 {
			v += 360
		}
		return v
	}
	h1, h2 := h(x.b, a1), h(y.b, a2)
	dl, dc := y.l-x.l, c2p-c1p
	var dh float64
	if c1p*c2p != 0 {
		dh = h2 - h1
		if dh > 180 {
			dh -= 360
		} else if dh < -180 {
			dh += 360
		}
	}
	dH := 2 * math.Sqrt(c1p*c2p) * math.Sin(rad(dh/2))
	lb, cbp := (x.l+y.l)/2, (c1p+c2p)/2
	var hb float64
	switch {
	case c1p*c2p == 0:
		hb = h1 + h2
	case math.Abs(h1-h2) <= 180:
		hb = (h1 + h2) / 2
	case h1+h2 < 360:
		hb = (h1 + h2 + 360) / 2
	default:
		hb = (h1 + h2 - 360) / 2
	}
	t := 1 - 0.17*math.Cos(rad(hb-30)) + 0.24*math.Cos(rad(2*hb)) + 0.32*math.Cos(rad(3*hb+6)) - 0.20*math.Cos(rad(4*hb-63))
	dth := 30 * math.Exp(-math.Pow((hb-275)/25, 2))
	rc := 2 * math.Sqrt(math.Pow(cbp, 7)/(math.Pow(cbp, 7)+math.Pow(25, 7)))
	sl := 1 + 0.015*math.Pow(lb-50, 2)/math.Sqrt(20+math.Pow(lb-50, 2))
	sc := 1 + 0.045*cbp
	sh := 1 + 0.015*cbp*t
	rt := -math.Sin(rad(2*dth)) * rc
	return math.Sqrt(math.Pow(dl/sl, 2) + math.Pow(dc/sc, 2) + math.Pow(dH/sh, 2) + rt*(dc/sc)*(dH/sh))
}

// vision is a kind of colour-vision deficiency simulated by the colour-blindness check.
type vision int

const (
	protanopia vision = iota
	deuteranopia
	tritanopia
)

var visionNames = [...]string{"protanopia", "deuteranopia", "tritanopia"}

func (v vision) String() string { return visionNames[v] }

// machado holds the Machado, Oliveira and Fernandes (2009) matrices at severity 1.0, applied in
// linear RGB.
var machado = [...][3][3]float64{
	protanopia:   {{0.152286, 1.052583, -0.204868}, {0.114503, 0.786281, 0.099216}, {-0.003882, -0.048116, 1.051998}},
	deuteranopia: {{0.367322, 0.860646, -0.227968}, {0.280085, 0.672501, 0.047413}, {-0.011820, 0.042940, 0.968881}},
	tritanopia:   {{1.255528, -0.076749, -0.178779}, {-0.078411, 0.930809, 0.147602}, {0.004733, 0.691367, 0.303900}},
}

// simulate is the colour as a person with the deficiency sees it, in Lab.
func (c rgb) simulate(v vision) lab {
	l, m := c.linear(), machado[v]
	var o [3]float64
	for i := range o {
		o[i] = math.Max(0, math.Min(1, m[i][0]*l[0]+m[i][1]*l[1]+m[i][2]*l[2]))
	}
	return labOfLinear(o[0], o[1], o[2])
}
