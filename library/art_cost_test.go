package library

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

func TestDecodeCost(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewYCbCr(image.Rect(0, 0, 160, 80), image.YCbCrSubsampleRatio420), nil)
	pic := buf.Bytes()
	cfg, format, _ := image.DecodeConfig(bytes.NewReader(pic))
	if got, want := decodeCost(pic, format, cfg), int64(160*80*3/2); got != want {
		t.Errorf("baseline 4:2:0: %d, want %d", got, want)
	}
	prog := bytes.Replace(pic, []byte{0xFF, 0xC0}, []byte{0xFF, 0xC2}, 1)
	if got, want := decodeCost(prog, format, cfg), int64(160*80*3/2*5); got != want {
		t.Errorf("progressive 4:2:0: %d, want %d", got, want)
	}
	if got := decodeCost([]byte{0xFF, 0xD8, 0xFF, 0xDA, 0, 2}, "jpeg", cfg); got != 24*160*80 {
		t.Errorf("no frame header: %d, want the worst case", got)
	}
	// Over the side limit is refused before any decode; within it, by cost.
	if 12000*12000*3/2 > thumbMaxBytes || 12000*12000*15/2 <= thumbMaxBytes {
		t.Error("the budget no longer matches its comment")
	}
}
