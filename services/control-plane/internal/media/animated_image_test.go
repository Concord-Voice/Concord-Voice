package media_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image/gif"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// IsAnimatedImage is the shared "would a browser animate this?" check behind
// the inline server icon/banner gate (#3856). It keys on the bytes, never on a
// declared MIME type, and fails closed on anything it cannot classify.

func TestIsAnimatedImageClassifiesByContent(t *testing.T) {
	for name, tc := range map[string]struct {
		raw      []byte
		animated bool
	}{
		"static png":        {testhelpers.StaticPNGBytes(t), false},
		"png without actl":  {testhelpers.APNGBytes(t, false), false},
		"apng":              {testhelpers.APNGBytes(t, true), true},
		"jpeg":              {testhelpers.StaticJPEGBytes(t), false},
		"single frame gif":  {testhelpers.GIFBytes(t, 1), false},
		"multi frame gif":   {testhelpers.GIFBytes(t, 2), true},
		"static webp":       {testhelpers.WebPBytes(false), false},
		"animated webp":     {testhelpers.WebPBytes(true), true},
		"webp anim chunk":   {webpWithoutAnimationFlag(), true},
		"webp animation on": {webpFlagOnly(), true},
		// Past the upload pipeline's 120-frame budget: still animated, not malformed.
		"gif past the upload frame budget": {testhelpers.GIFBytes(t, 121), true},
		"webp odd chunk before anim":       {webpOddChunkBeforeANIM(), true},
		"png actl after idat":              {pngACTLAfterIDAT(t), false},
		// Go's decoder sees one frame here; browsers see two and animate it.
		"gif frame hidden from go decoder": {testhelpers.GIFWithFrameHiddenFromGoDecoder(t), true},
	} {
		t.Run(name, func(t *testing.T) {
			animated, err := media.IsAnimatedImage(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, tc.animated, animated)
		})
	}
}

// The fixture is only a regression test if Go's decoder really disagrees with
// the browser grammar on it; this pins that, so a Go fix does not leave the
// case above silently testing nothing new.
func TestGIFFixtureHidesAFrameFromGoDecoder(t *testing.T) {
	g, err := gif.DecodeAll(bytes.NewReader(testhelpers.GIFWithFrameHiddenFromGoDecoder(t)))
	require.NoError(t, err)
	assert.Len(t, g.Image, 1)
}

func TestIsAnimatedImageRejectsWhatItCannotClassify(t *testing.T) {
	gifBytes := testhelpers.GIFBytes(t, 3)
	for name, raw := range map[string][]byte{
		"nil":                nil,
		"empty":              {},
		"text":               []byte("not an image"),
		"svg":                []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"gif signature only": []byte("GIF89a"),
		"gif with no image":  []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00\x3b"),
		// An empty graphic control block followed by a trailer byte: a decoder
		// that reads 4 bytes regardless takes the 3B as payload and keeps going.
		"gif short graphic control":  gifWithShortGraphicControl(t),
		"gif second frame truncated": gifWithTruncatedSecondFrame(t),
		"png idat overruns":          cutInsideChunk(t, testhelpers.StaticPNGBytes(t), "IDAT"),
		"png actl overruns":          cutInsideChunk(t, testhelpers.APNGBytes(t, true), "acTL"),
		"truncated gif":              gifBytes[:len(gifBytes)/2],
		"png signature only":         []byte("\x89PNG\r\n\x1a\n"),
		"png cut before idat":        testhelpers.APNGBytes(t, false)[:33],
		"png chunk past the end":     pngWithOversizedChunk(),
		"png iend before idat":       pngEndingBeforeIDAT(),
		"riff wave":                  []byte("RIFF\x04\x00\x00\x00WAVE\x00\x00\x00\x00"),
		"webp header only":           []byte("RIFF\x04\x00\x00\x00WEBP"),
		"webp chunk past the end":    testhelpers.WebPBytes(true)[:20],
	} {
		t.Run(name, func(t *testing.T) {
			animated, err := media.IsAnimatedImage(raw)
			require.Error(t, err)
			assert.False(t, animated)
		})
	}
}

// webpWithoutAnimationFlag is a VP8X header with the animation flag CLEAR but
// an ANIM chunk present: the chunk alone must still count as animated.
func webpWithoutAnimationFlag() []byte {
	animated := testhelpers.WebPBytes(true)
	out := append([]byte{}, animated...)
	out[20] = 0x00 // VP8X flags byte: 12-byte RIFF header + 8-byte chunk header
	return out
}

// webpFlagOnly is a VP8X header with the animation flag SET and no ANIM chunk.
func webpFlagOnly() []byte {
	static := testhelpers.WebPBytes(false)
	out := append([]byte{}, static...)
	out[20] = 0x02
	return out
}

// pngEndingBeforeIDAT is a PNG whose IEND arrives before any IDAT.
func pngEndingBeforeIDAT() []byte {
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, 0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0, 0, 0, 0)
	return append(out, 0, 0, 0, 0, 'I', 'E', 'N', 'D', 0, 0, 0, 0)
}

// pngWithOversizedChunk declares a tEXt chunk far longer than the data that follows.
func pngWithOversizedChunk() []byte {
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, 0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0, 0, 0, 0)
	return append(out, 0x7f, 0xff, 0xff, 0xff, 't', 'E', 'X', 't', 'a', 'b')
}

// webpOddChunkBeforeANIM puts an odd-length chunk, and its pad byte, between a
// flag-clear VP8X header and an ANIM chunk. A walker that skips the pad byte
// lands mid-chunk and never reaches the ANIM.
func webpOddChunkBeforeANIM() []byte {
	out := []byte("RIFF\x00\x00\x00\x00WEBP")
	out = append(out, "VP8X\x0a\x00\x00\x00"...)
	out = append(out, make([]byte, 10)...)
	out = append(out, "EXIF\x03\x00\x00\x00abc\x00"...)
	out = append(out, "ANIM\x06\x00\x00\x00"...)
	out = append(out, make([]byte, 6)...)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8)) // #nosec G115 -- a few dozen bytes
	return out
}

// pngACTLAfterIDAT is a static PNG with an acTL chunk after its image data.
// Browsers ignore a late acTL, so it must classify as static.
func pngACTLAfterIDAT(t *testing.T) []byte {
	base := testhelpers.StaticPNGBytes(t)
	const iendLen = 12
	chunk := binary.BigEndian.AppendUint32(nil, 8)
	chunk = append(chunk, "acTL"...)
	chunk = append(chunk, 0, 0, 0, 2, 0, 0, 0, 0)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := append([]byte{}, base[:len(base)-iendLen]...)
	out = append(out, chunk...)
	return append(out, base[len(base)-iendLen:]...)
}

// gifWithShortGraphicControl appends a zero-size graphic control block, a
// trailer byte, three filler bytes and a second frame to a one-frame GIF.
func gifWithShortGraphicControl(t *testing.T) []byte {
	one := testhelpers.GIFBytes(t, 1)
	two := testhelpers.GIFBytes(t, 2)
	out := append([]byte{}, one[:len(one)-1]...) // drop the trailer
	out = append(out, 0x21, 0xF9, 0x00, 0x3B, 0, 0, 0, 0x00)
	// The second frame of `two`, from its image descriptor through the trailer.
	return append(out, two[bytes.LastIndex(two, []byte{0x2C, 0, 0, 0, 0, 2, 0, 2, 0}):]...)
}

// gifWithTruncatedSecondFrame is a valid one-frame GIF followed by the start of
// a second image descriptor and nothing else.
func gifWithTruncatedSecondFrame(t *testing.T) []byte {
	one := testhelpers.GIFBytes(t, 1)
	out := append([]byte{}, one[:len(one)-1]...) // drop the trailer
	return append(out, 0x2C, 0, 0, 0, 0)
}

// cutInsideChunk truncates a PNG two bytes into the data of the named chunk,
// so the chunk's declared length overruns what is left.
func cutInsideChunk(t *testing.T, png []byte, chunkType string) []byte {
	i := bytes.Index(png, []byte(chunkType))
	require.Positive(t, i, "fixture has no %s chunk", chunkType)
	return append([]byte{}, png[:i+len(chunkType)+2]...)
}
