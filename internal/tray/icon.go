package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// IconICO returns a 32x32 PNG-in-ICO icon for the system tray:
// Telegram-blue circle with a white capital "T".
func IconICO() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	blue := color.RGBA{0x22, 0x9E, 0xD9, 0xFF}
	white := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}

	cx, cy, r := 15.5, 15.5, 15.5
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, blue)
			}
		}
	}
	for y := 7; y <= 11; y++ {
		for x := 7; x <= 24; x++ {
			img.Set(x, y, white)
		}
	}
	for y := 12; y <= 24; y++ {
		for x := 13; x <= 18; x++ {
			img.Set(x, y, white)
		}
	}

	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	pngData := pngBuf.Bytes()

	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, uint16(0))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	ico.WriteByte(32)
	ico.WriteByte(32)
	ico.WriteByte(0)
	ico.WriteByte(0)
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	binary.Write(&ico, binary.LittleEndian, uint16(32))
	binary.Write(&ico, binary.LittleEndian, uint32(len(pngData)))
	binary.Write(&ico, binary.LittleEndian, uint32(22))
	ico.Write(pngData)
	return ico.Bytes()
}
