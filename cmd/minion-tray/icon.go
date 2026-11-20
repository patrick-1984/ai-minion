//go:build !(darwin && !cgo) && !(linux && arm)

package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"runtime"
)

var (
	colorGreen = color.RGBA{0x2e, 0xa0, 0x43, 0xff}
	colorGray  = color.RGBA{0x80, 0x80, 0x80, 0xff}
	colorRed   = color.RGBA{0xd7, 0x30, 0x30, 0xff}
)

// iconBytes draws a filled circle: PNG, wrapped in an ICO container on Windows.
func iconBytes(c color.RGBA) []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := 2*x-n+1, 2*y-n+1
			if dx*dx+dy*dy <= (n-2)*(n-2) {
				img.SetRGBA(x, y, c)
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	if runtime.GOOS != "windows" {
		return b.Bytes()
	}
	p := b.Bytes()
	var ico bytes.Buffer
	_ = binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})
	ico.Write([]byte{n, n, 0, 0})
	_ = binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
	_ = binary.Write(&ico, binary.LittleEndian, []uint32{uint32(len(p)), 22})
	ico.Write(p)
	return ico.Bytes()
}
