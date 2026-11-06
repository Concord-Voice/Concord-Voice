package media_test

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
)

func encodeGIF(t *testing.T, frames, side int) []byte {
	t.Helper()
	anim := &gif.GIF{}
	for i := 0; i < frames; i++ {
		anim.Image = append(anim.Image, image.NewPaletted(image.Rect(0, 0, side, side), color.Palette{color.Black, color.White}))
		anim.Delay = append(anim.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckInlineAnimatedBudget(t *testing.T) {
	assert.NoError(t, media.CheckInlineAnimatedBudget(encodeGIF(t, 3, 2)), "a small animated GIF passes")
	assert.Error(t, media.CheckInlineAnimatedBudget(encodeGIF(t, 121, 2)), "over the frame cap")
	assert.Error(t, media.CheckInlineAnimatedBudget([]byte("GIF89a")), "truncated GIF")

	var p bytes.Buffer
	if err := png.Encode(&p, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	assert.Error(t, media.CheckInlineAnimatedBudget(p.Bytes()), "non-GIF animations are refused inline")
}
