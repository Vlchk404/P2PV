//go:build ignore

// Generates the tray icons as .ico files.
//
// Run from the repository root:
//
//	go run scripts/gen-icons.go
//
// The icons are drawn in code rather than committed as opaque binaries so the
// shapes stay reviewable and editable. Output goes to internal/tray/icons/.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

const size = 32

type rgba struct{ r, g, b, a uint8 }

var (
	transparent = rgba{0, 0, 0, 0}
	white       = rgba{255, 255, 255, 255}
	green       = rgba{64, 200, 96, 255}   // connected, direct path
	amber       = rgba{230, 170, 40, 255}  // connected via relay
	grey        = rgba{130, 138, 148, 255} // not connected
)

func main() {
	out := filepath.Join("internal", "tray", "icons")
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	icons := map[string]rgba{
		"connected.ico": green,
		"relayed.ico":   amber,
		"offline.ico":   grey,
	}

	for name, colour := range icons {
		data, err := encodeICO(draw(colour))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		path := filepath.Join(out, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(data))
	}

	// The application icon, for the .exe itself. go-winres reads PNG, and it
	// scales one source image down to every size Windows asks for, so this is
	// drawn larger than the tray icons.
	if err := writePNG(filepath.Join("winres", "icon.png"), green); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// writePNG renders the same artwork as a PNG for the executable's icon.
func writePNG(path string, accent rgba) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	px := draw(accent)
	const scale = 8 // 32 -> 256, the largest size Windows uses
	img := image.NewNRGBA(image.Rect(0, 0, size*scale, size*scale))
	for y := 0; y < size*scale; y++ {
		for x := 0; x < size*scale; x++ {
			p := px[(y/scale)*size+(x/scale)]
			img.Set(x, y, color.NRGBA{R: p.r, G: p.g, B: p.b, A: p.a})
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%dx%d)\n", path, size*scale, size*scale)
	return nil
}

// draw paints two linked nodes: the picture of a peer-to-peer connection, and
// legible at 16x16 once the system scales it down.
func draw(accent rgba) []rgba {
	px := make([]rgba, size*size)
	for i := range px {
		px[i] = transparent
	}

	// Rounded background square in the accent colour.
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if inRoundedRect(x, y, 1, 1, size-2, size-2, 7) {
				px[y*size+x] = accent
			}
		}
	}

	// Two white nodes on a diagonal, joined by a bar: A <-> B.
	disc(px, 11, 11, 4, white)
	disc(px, 21, 21, 4, white)
	for t := 0; t <= 10; t++ {
		x, y := 11+t, 11+t
		disc(px, x, y, 1, white)
	}

	return px
}

func inRoundedRect(x, y, rx, ry, w, h, r int) bool {
	if x < rx || y < ry || x >= rx+w || y >= ry+h {
		return false
	}
	// Corner cut-outs.
	corners := [][2]int{
		{rx + r, ry + r}, {rx + w - 1 - r, ry + r},
		{rx + r, ry + h - 1 - r}, {rx + w - 1 - r, ry + h - 1 - r},
	}
	inX := x < rx+r || x > rx+w-1-r
	inY := y < ry+r || y > ry+h-1-r
	if !inX || !inY {
		return true
	}
	for _, c := range corners {
		dx, dy := x-c[0], y-c[1]
		if dx*dx+dy*dy <= r*r {
			return true
		}
	}
	return false
}

func disc(px []rgba, cx, cy, r int, c rgba) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if x < 0 || y < 0 || x >= size || y >= size {
				continue
			}
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				px[y*size+x] = c
			}
		}
	}
}

// encodeICO writes a single-image .ico holding an uncompressed 32-bit BMP.
//
// The classic DIB form is used rather than an embedded PNG: both are valid, but
// every Windows version reads the DIB form, and the tray is the one place where
// a silently blank icon would be hard to notice.
func encodeICO(px []rgba) ([]byte, error) {
	var dib bytes.Buffer

	// BITMAPINFOHEADER. Height is doubled: the DIB holds the colour rows and
	// then the AND mask rows.
	writeAll(&dib,
		uint32(40),         // biSize
		int32(size),        // biWidth
		int32(size*2),      // biHeight
		uint16(1),          // biPlanes
		uint16(32),         // biBitCount
		uint32(0),          // biCompression = BI_RGB
		uint32(0),          // biSizeImage
		int32(0), int32(0), // resolution
		uint32(0), uint32(0), // palette
	)

	// Colour rows, bottom-up, as BGRA.
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			p := px[y*size+x]
			dib.Write([]byte{p.b, p.g, p.r, p.a})
		}
	}

	// AND mask: one bit per pixel, rows padded to 4 bytes. The alpha channel
	// already carries transparency, so this is zeroed -- but it must be present
	// and correctly sized or the image is rejected.
	maskRow := ((size + 31) / 32) * 4
	dib.Write(make([]byte, maskRow*size))

	var ico bytes.Buffer
	writeAll(&ico,
		uint16(0), uint16(1), uint16(1), // reserved, type=icon, one image
		uint8(size), uint8(size), // width, height
		uint8(0), uint8(0), // palette count, reserved
		uint16(1), uint16(32), // planes, bit count
		uint32(dib.Len()), // bytes of image data
		uint32(6+16),      // offset: both headers
	)
	ico.Write(dib.Bytes())
	return ico.Bytes(), nil
}

func writeAll(buf *bytes.Buffer, values ...any) {
	for _, v := range values {
		if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
}
