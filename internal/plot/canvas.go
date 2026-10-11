package plot

import (
	"bytes"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

type anchor int

const (
	anchorStart anchor = iota
	anchorMiddle
	anchorEnd
)

// canvas abstracts the two output formats so layout code is written once.
type canvas interface {
	Rect(x, y, w, h float64, fill color.RGBA)
	Line(x1, y1, x2, y2 float64, c color.RGBA, width float64, dashed bool)
	Text(x, y float64, s string, c color.RGBA, a anchor)
	Bytes() ([]byte, error)
}

// ---------------------------------------------------------------- SVG

type svgCanvas struct {
	b    strings.Builder
	w, h int
}

func newSVG(w, h int) *svgCanvas {
	c := &svgCanvas{w: w, h: h}
	fmt.Fprintf(&c.b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="monospace" font-size="11">`+"\n", w, h, w, h)
	return c
}

func hexColor(c color.RGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

func (c *svgCanvas) Rect(x, y, w, h float64, fill color.RGBA) {
	fmt.Fprintf(&c.b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"`, x, y, w, h, hexColor(fill))
	if fill.A != 255 {
		fmt.Fprintf(&c.b, ` fill-opacity="%.2f"`, float64(fill.A)/255)
	}
	c.b.WriteString("/>\n")
}

func (c *svgCanvas) Line(x1, y1, x2, y2 float64, col color.RGBA, width float64, dashed bool) {
	fmt.Fprintf(&c.b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="%.2f"`, x1, y1, x2, y2, hexColor(col), width)
	if dashed {
		c.b.WriteString(` stroke-dasharray="4 3"`)
	}
	c.b.WriteString("/>\n")
}

func (c *svgCanvas) Text(x, y float64, s string, col color.RGBA, a anchor) {
	anc := [...]string{"start", "middle", "end"}[a]
	fmt.Fprintf(&c.b, `<text x="%.2f" y="%.2f" fill="%s" text-anchor="%s">%s</text>`+"\n", x, y, hexColor(col), anc, html.EscapeString(s))
}

func (c *svgCanvas) Bytes() ([]byte, error) {
	return []byte(c.b.String() + "</svg>\n"), nil
}

// ---------------------------------------------------------------- PNG

type pngCanvas struct{ img *image.RGBA }

func newPNG(w, h int) *pngCanvas {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	return &pngCanvas{img: img}
}

func (c *pngCanvas) blend(x, y int, col color.RGBA) {
	if !(image.Point{x, y}).In(c.img.Rect) {
		return
	}
	if col.A == 255 {
		c.img.SetRGBA(x, y, col)
		return
	}
	d := c.img.RGBAAt(x, y)
	a := uint32(col.A)
	mix := func(s, d uint8) uint8 { return uint8((uint32(s)*a + uint32(d)*(255-a)) / 255) }
	c.img.SetRGBA(x, y, color.RGBA{mix(col.R, d.R), mix(col.G, d.G), mix(col.B, d.B), 255})
}

func (c *pngCanvas) Rect(x, y, w, h float64, fill color.RGBA) {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := int(math.Ceil(x+w)), int(math.Ceil(y+h))
	if x1 <= x0 {
		x1 = x0 + 1
	}
	if y1 <= y0 {
		y1 = y0 + 1
	}
	for yy := y0; yy < y1; yy++ {
		for xx := x0; xx < x1; xx++ {
			c.blend(xx, yy, fill)
		}
	}
}

func (c *pngCanvas) Line(x1, y1, x2, y2 float64, col color.RGBA, width float64, dashed bool) {
	dx, dy := x2-x1, y2-y1
	steps := int(math.Max(math.Abs(dx), math.Abs(dy)))
	if steps == 0 {
		steps = 1
	}
	half := int(math.Max(0, math.Round(width)-1)) / 2
	for i := 0; i <= steps; i++ {
		if dashed && (i/4)%2 == 1 {
			continue
		}
		t := float64(i) / float64(steps)
		px, py := int(math.Round(x1+dx*t)), int(math.Round(y1+dy*t))
		for oy := -half; oy <= half; oy++ {
			for ox := -half; ox <= half; ox++ {
				c.blend(px+ox, py+oy, col)
			}
		}
	}
}

func (c *pngCanvas) Text(x, y float64, s string, col color.RGBA, a anchor) {
	face := basicfont.Face7x13
	s = strings.NewReplacer("µ", "u", "→", "->").Replace(s) // basicfont has no such glyphs
	d := &font.Drawer{Dst: c.img, Src: image.NewUniform(col), Face: face}
	w := d.MeasureString(s).Round()
	switch a {
	case anchorMiddle:
		x -= float64(w) / 2
	case anchorEnd:
		x -= float64(w)
	}
	d.Dot = fixed.P(int(x), int(y))
	d.DrawString(s)
}

func (c *pngCanvas) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, c.img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
