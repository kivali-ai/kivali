package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Plainsong's mark: a cream ring on slate blue, with three short
// lines inside it, like the rows of a rota. Drawn procedurally at each
// size the app serves, so the bytes are the same on every run and
// nothing is downscaled.
var (
	logoGround = color.RGBA{R: 0x33, G: 0x45, B: 0x66, A: 0xff}
	logoInk    = color.RGBA{R: 0xf4, G: 0xee, B: 0xe2, A: 0xff}
)

// renderLogo draws the mark at size×size.
func renderLogo(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	n := float64(size)
	c := n / 2
	outer, inner := 0.40*n, 0.32*n
	// Three rows, each a band centred on the mark.
	rows := []struct{ top, half float64 }{{0.38, 0.14}, {0.47, 0.18}, {0.56, 0.10}}
	row := func(fx, fy float64) bool {
		for _, r := range rows {
			if fy >= r.top*n && fy < (r.top+0.05)*n && fx >= c-r.half*n && fx < c+r.half*n {
				return true
			}
		}
		return false
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			d := math.Hypot(fx-c, fy-c)
			col := logoGround
			if (d <= outer && d >= inner) || row(fx, fy) {
				col = logoInk
			}
			img.SetRGBA(x, y, col)
		}
	}
	return img
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// logoAssets builds the favicon set store.WriteFaviconAssets takes,
// under the names the app serves (the same set internal/web's upload
// handler derives from an uploaded PNG): icon-32/180/512.png and a
// favicon.ico holding PNG-encoded 16/32/48 entries.
func logoAssets() (map[string][]byte, error) {
	assets := map[string][]byte{}
	for _, size := range []int{32, 180, 512} {
		b, err := encodePNG(renderLogo(size))
		if err != nil {
			return nil, err
		}
		assets[fmt.Sprintf("icon-%d.png", size)] = b
	}
	sizes := []int{16, 32, 48}
	var pngs [][]byte
	for _, size := range sizes {
		b, err := encodePNG(renderLogo(size))
		if err != nil {
			return nil, err
		}
		pngs = append(pngs, b)
	}
	assets["favicon.ico"] = encodeICO(sizes, pngs)
	return assets, nil
}

// encodeICO assembles an .ico whose entries embed PNG data verbatim.
func encodeICO(sizes []int, pngs [][]byte) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(pngs)))
	offset := 6 + 16*len(pngs)
	for i, p := range pngs {
		buf.WriteByte(byte(sizes[i]))                           // width
		buf.WriteByte(byte(sizes[i]))                           // height
		buf.WriteByte(0)                                        // palette size
		buf.WriteByte(0)                                        // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // color planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bits per pixel
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(p)))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(p)
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes()
}
