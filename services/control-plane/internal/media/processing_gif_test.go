package media

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// All GIF fixtures are generated in-test via gif.EncodeAll — no binary files.

var (
	gifColRed   = color.RGBA{R: 255, A: 255}
	gifColBlue  = color.RGBA{B: 255, A: 255}
	gifColWhite = color.RGBA{R: 255, G: 255, B: 255, A: 255}
	gifColClear = color.RGBA{}
)

// gifTestPalette is shared by every generated frame, mirroring the common
// real-world case where all frames reference the global color table.
func gifTestPalette() color.Palette {
	return color.Palette{gifColClear, gifColRed, gifColBlue, gifColWhite}
}

// solidPalettedFrame creates a paletted frame covering rect filled with c.
func solidPalettedFrame(t *testing.T, rect image.Rectangle, c color.Color, pal color.Palette) *image.Paletted {
	t.Helper()
	frame := image.NewPaletted(rect, pal)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			frame.Set(x, y, c)
		}
	}
	return frame
}

// encodeTestGIF assembles frames into an animated GIF byte stream.
func encodeTestGIF(t *testing.T, frames []*image.Paletted, delays []int, disposal []byte, loopCount, screenW, screenH int) []byte {
	t.Helper()
	g := &gif.GIF{
		Image:     frames,
		Delay:     delays,
		Disposal:  disposal,
		LoopCount: loopCount,
		Config:    image.Config{Width: screenW, Height: screenH},
	}
	var buf bytes.Buffer
	require.NoError(t, gif.EncodeAll(&buf, g))
	return buf.Bytes()
}

// gifWithFakeFrameDescriptors hand-builds a minimal GIF89a byte stream with
// `count` image descriptors that DECLARE frameW×frameH dims but carry an empty
// (single-terminator) image-data sub-block chain. It is deliberately NOT a
// decodable animation — it exercises the pre-decode container scan, which reads
// only the descriptor structure, not pixel data.
func gifWithFakeFrameDescriptors(frameW, frameH, count int) []byte {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	// Logical Screen Descriptor: width, height (LE uint16), packed=0 (no GCT),
	// bg color index, pixel aspect ratio.
	le16 := func(v int) {
		b.WriteByte(byte(v & 0xFF))
		b.WriteByte(byte((v >> 8) & 0xFF))
	}
	le16(frameW)
	le16(frameH)
	b.WriteByte(0x00) // packed: no global color table
	b.WriteByte(0x00) // background color index
	b.WriteByte(0x00) // pixel aspect ratio
	for i := 0; i < count; i++ {
		b.WriteByte(0x2C) // Image Descriptor introducer
		le16(0)           // left
		le16(0)           // top
		le16(frameW)      // width
		le16(frameH)      // height
		b.WriteByte(0x00) // packed: no local color table
		b.WriteByte(0x08) // LZW minimum code size
		b.WriteByte(0x00) // empty image data: immediate sub-block terminator
	}
	b.WriteByte(0x3B) // Trailer
	return b.Bytes()
}

// assertGIFPixel asserts the 8-bit RGBA value of a pixel in a decoded frame.
func assertGIFPixel(t *testing.T, img image.Image, x, y int, want color.RGBA) {
	t.Helper()
	got, ok := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	require.True(t, ok, "pixel (%d,%d) converts to RGBA", x, y)
	assert.Equal(t, want, got, "pixel (%d,%d)", x, y)
}

func decodeProcessedGIF(t *testing.T, p *ProcessedImage) *gif.GIF {
	t.Helper()
	out, err := gif.DecodeAll(bytes.NewReader(p.Data))
	require.NoError(t, err)
	return out
}

// =====================================================================
// ProcessImageGIF — animation-preserving pipeline
// =====================================================================

func TestProcessImageGIF_PreservesTimelineAndResizes(t *testing.T) {
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 100, 100), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 100, 100), gifColBlue, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 100, 100), gifColWhite, pal),
	}
	raw := encodeTestGIF(t, frames, []int{10, 20, 30},
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalNone}, 2, 100, 100)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 50, 50)
	require.NoError(t, err)
	assert.Equal(t, "image/gif", got.ContentType)
	assert.Equal(t, 50, got.Width)
	assert.Equal(t, 50, got.Height)

	out := decodeProcessedGIF(t, got)
	assert.Len(t, out.Image, 3, "frame count preserved")
	assert.Equal(t, []int{10, 20, 30}, out.Delay, "per-frame delays preserved")
	assert.Equal(t, 2, out.LoopCount, "loop count preserved")
	for i, fr := range out.Image {
		b := fr.Bounds()
		assert.Equal(t, 50, b.Dx(), "frame %d width", i)
		assert.Equal(t, 50, b.Dy(), "frame %d height", i)
	}
	for i, d := range out.Disposal {
		assert.Equal(t, byte(gif.DisposalNone), d, "output disposal %d is none (frames are coalesced)", i)
	}
}

func TestProcessImageGIF_NoUpscaleWhenWithinBounds(t *testing.T) {
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 40, 30), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 40, 30), gifColBlue, pal),
	}
	raw := encodeTestGIF(t, frames, []int{5, 5}, []byte{gif.DisposalNone, gif.DisposalNone}, 0, 40, 30)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 512, 512)
	require.NoError(t, err)
	assert.Equal(t, 40, got.Width)
	assert.Equal(t, 30, got.Height)
	assert.Len(t, decodeProcessedGIF(t, got).Image, 2)
}

// Optimized GIFs ship partial frames; the pipeline must composite each frame
// onto the logical canvas so a naive per-frame resize can't glitch.
func TestProcessImageGIF_CoalescesPartialFrames(t *testing.T) {
	pal := gifTestPalette()
	f1 := solidPalettedFrame(t, image.Rect(0, 0, 8, 8), gifColRed, pal)
	f2 := solidPalettedFrame(t, image.Rect(4, 4, 8, 8), gifColBlue, pal) // partial frame
	raw := encodeTestGIF(t, []*image.Paletted{f1, f2}, []int{5, 5},
		[]byte{gif.DisposalNone, gif.DisposalNone}, 0, 8, 8)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 8, 8) // no resize → exact pixels
	require.NoError(t, err)

	out := decodeProcessedGIF(t, got)
	require.Len(t, out.Image, 2)
	// Frame 2 must be a full logical frame: frame-1 pixels survive outside the
	// partial frame-2 region, frame-2 pixels land inside it.
	assertGIFPixel(t, out.Image[1], 0, 0, gifColRed)
	assertGIFPixel(t, out.Image[1], 5, 5, gifColBlue)
}

func TestProcessImageGIF_HonorsDisposalBackground(t *testing.T) {
	pal := gifTestPalette()
	f1 := solidPalettedFrame(t, image.Rect(0, 0, 8, 8), gifColRed, pal)
	f2 := solidPalettedFrame(t, image.Rect(0, 0, 4, 4), gifColBlue, pal)
	raw := encodeTestGIF(t, []*image.Paletted{f1, f2}, []int{5, 5},
		[]byte{gif.DisposalBackground, gif.DisposalNone}, 0, 8, 8)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 8, 8)
	require.NoError(t, err)

	out := decodeProcessedGIF(t, got)
	require.Len(t, out.Image, 2)
	// Frame 1 disposed to background → its red must NOT survive under frame 2.
	assertGIFPixel(t, out.Image[1], 1, 1, gifColBlue)
	assertGIFPixel(t, out.Image[1], 6, 6, gifColClear)
}

func TestProcessImageGIF_HonorsDisposalPrevious(t *testing.T) {
	pal := gifTestPalette()
	f1 := solidPalettedFrame(t, image.Rect(0, 0, 8, 8), gifColRed, pal)
	f2 := solidPalettedFrame(t, image.Rect(4, 4, 8, 8), gifColBlue, pal)
	f3 := solidPalettedFrame(t, image.Rect(0, 0, 2, 2), gifColWhite, pal)
	raw := encodeTestGIF(t, []*image.Paletted{f1, f2, f3}, []int{5, 5, 5},
		[]byte{gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone}, 0, 8, 8)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 8, 8)
	require.NoError(t, err)

	out := decodeProcessedGIF(t, got)
	require.Len(t, out.Image, 3)
	// Frame 2 shows its blue region…
	assertGIFPixel(t, out.Image[1], 5, 5, gifColBlue)
	// …but frame 2's disposal is "previous": by frame 3 the blue region is
	// restored to frame 1's red, and frame 3 adds white at the origin.
	assertGIFPixel(t, out.Image[2], 5, 5, gifColRed)
	assertGIFPixel(t, out.Image[2], 0, 0, gifColWhite)
}

// A frame with a LOCAL color table that omits an earlier frame's survivor color
// must not corrupt those survivor pixels. Frame 1 fills 8×8 red (DisposalNone);
// frame 2 is a 2×2 blue patch at the origin carrying a local palette of only
// {transparent, blue}. Palettizing the coalesced frame-2 canvas against that
// local palette would remap the surviving red to the nearest local color; the
// accumulated cross-frame palette keeps the red intact.
func TestProcessImageGIF_LocalPaletteFrameDoesNotCorruptSurvivors(t *testing.T) {
	globalPal := gifTestPalette()
	f1 := solidPalettedFrame(t, image.Rect(0, 0, 8, 8), gifColRed, globalPal)

	// Frame 2: 2×2 blue at origin, LOCAL palette without red.
	localPal := color.Palette{gifColClear, gifColBlue}
	f2 := solidPalettedFrame(t, image.Rect(0, 0, 2, 2), gifColBlue, localPal)

	raw := encodeTestGIF(t, []*image.Paletted{f1, f2}, []int{5, 5},
		[]byte{gif.DisposalNone, gif.DisposalNone}, 0, 8, 8)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 8, 8) // no resize → exact pixels
	require.NoError(t, err)

	out := decodeProcessedGIF(t, got)
	require.Len(t, out.Image, 2)
	// Survivor red outside the 2×2 patch must remain red, not be remapped to a
	// frame-2-local color.
	assertGIFPixel(t, out.Image[1], 6, 6, gifColRed)
	// The blue patch stays blue.
	assertGIFPixel(t, out.Image[1], 0, 0, gifColBlue)
}

// =====================================================================
// accumulateGIFPalette — transparent-slot conditioning (#1302 Gitar)
// =====================================================================

// opaque256Palette builds a palette of exactly 256 distinct, fully-opaque
// colors (no transparent entry). Index i encodes i across R/G/B so every entry
// is unique and none has alpha 0.
func opaque256Palette() color.Palette {
	pal := make(color.Palette, 256)
	for i := 0; i < 256; i++ {
		pal[i] = color.RGBA{
			R: uint8(i),
			G: uint8((i * 7) & 0xFF),
			B: uint8((i * 13) & 0xFF),
			A: 255,
		}
	}
	return pal
}

// A fully-opaque animation that collectively uses exactly 256 distinct colors
// must keep its real 256-entry accumulated palette — NOT overflow to 257 (via a
// pre-seeded transparent slot) and drop to Plan9. Regression guard for the Gitar
// finding: the transparent slot is charged only when a frame actually uses
// transparency.
func TestAccumulateGIFPalette_Opaque256KeepsRealPalette(t *testing.T) {
	pal := opaque256Palette()
	f1 := solidPalettedFrame(t, image.Rect(0, 0, 4, 4), pal[10], pal)
	f2 := solidPalettedFrame(t, image.Rect(0, 0, 4, 4), pal[200], pal)

	acc := accumulateGIFPalette([]*image.Paletted{f1, f2})

	require.Len(t, acc, 256, "opaque 256-color union stays 256 (no pre-seeded transparent slot)")
	// The accumulated palette must be the real source colors, not Plan9's
	// web-safe quantization.
	for _, c := range pal {
		assert.Contains(t, acc, c, "source color survives into accumulated palette")
	}
	// No transparent entry was invented for a fully-opaque animation.
	for _, c := range acc {
		_, _, _, a := c.RGBA()
		assert.NotZero(t, a, "no fully-transparent entry in an opaque animation's palette")
	}
}

// End-to-end: a fully-opaque 256-color animation re-encodes with its real
// colors (no web-palette quantization), confirming accumulateGIFPalette did not
// fall back to Plan9.
func TestProcessImageGIF_Opaque256ColorKeepsSourceColors(t *testing.T) {
	pal := opaque256Palette()
	f1 := solidPalettedFrame(t, image.Rect(0, 0, 4, 4), pal[10], pal)
	f2 := solidPalettedFrame(t, image.Rect(0, 0, 4, 4), pal[200], pal)
	raw := encodeTestGIF(t, []*image.Paletted{f1, f2}, []int{5, 5},
		[]byte{gif.DisposalNone, gif.DisposalNone}, 0, 4, 4)

	got, err := ProcessImageGIF(bytes.NewReader(raw), 4, 4) // no resize → exact colors
	require.NoError(t, err)

	out := decodeProcessedGIF(t, got)
	require.Len(t, out.Image, 2)
	// Source colors survive exactly — Plan9 quantization would shift them.
	wantF1 := pal[10].(color.RGBA)
	wantF2 := pal[200].(color.RGBA)
	assertGIFPixel(t, out.Image[0], 2, 2, wantF1)
	assertGIFPixel(t, out.Image[1], 2, 2, wantF2)
}

// When a frame genuinely uses transparency, the accumulated palette still pins
// the transparent color at index 0 (so coalesced-transparent regions stay
// transparent, and the disposal-to-background path has a transparent slot).
func TestAccumulateGIFPalette_TransparencyPinnedAtIndexZero(t *testing.T) {
	// Frame palette: a real opaque color at index 0, transparent at index 1.
	pal := color.Palette{gifColRed, gifColClear, gifColBlue}
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), pal)
	frame.SetColorIndex(0, 0, 1) // paint a pixel with the transparent entry
	frame.SetColorIndex(1, 1, 0) // and a pixel with red

	acc := accumulateGIFPalette([]*image.Paletted{frame})

	require.NotEmpty(t, acc)
	_, _, _, a := acc[0].RGBA()
	assert.Zero(t, a, "transparent color pinned at accumulated index 0")
}

// =====================================================================
// safeDecodeGIF — decompression-bomb guards
// =====================================================================

func TestSafeDecodeGIF_AcceptsGuardCompliantAnimation(t *testing.T) {
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 10, 10), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 10, 10), gifColBlue, pal),
	}
	raw := encodeTestGIF(t, frames, []int{5, 5}, []byte{gif.DisposalNone, gif.DisposalNone}, 0, 10, 10)

	g, err := safeDecodeGIF(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Len(t, g.Image, 2)
}

func TestSafeDecodeGIF_RejectsFrameCountOverBudget(t *testing.T) {
	pal := gifTestPalette()
	frames := make([]*image.Paletted, maxGifFrames+1)
	delays := make([]int, maxGifFrames+1)
	disposal := make([]byte, maxGifFrames+1)
	for i := range frames {
		frames[i] = solidPalettedFrame(t, image.Rect(0, 0, 1, 1), gifColRed, pal)
		disposal[i] = gif.DisposalNone
	}
	raw := encodeTestGIF(t, frames, delays, disposal, 0, 1, 1)

	_, err := safeDecodeGIF(bytes.NewReader(raw))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frame count")
}

func TestSafeDecodeGIF_RejectsTotalPixelBudget(t *testing.T) {
	// 9 frames × 5000×5000 logical screen = 225,000,000 px > maxGifTotalPixels
	// (200,000,000). Each FRAME descriptor is 1×1, so the pre-decode scan (which
	// sums frame-descriptor areas) passes and the POST-decode guard — which keys
	// on frames × screen — is the rejecting layer here. The dedicated pre-scan
	// coverage (frame-descriptor pixel bomb) is a separate test; this exercises
	// the frames×screen defense-in-depth arm. Fixture stays tiny (1×1 frames).
	pal := gifTestPalette()
	frames := make([]*image.Paletted, 9)
	delays := make([]int, 9)
	disposal := make([]byte, 9)
	for i := range frames {
		frames[i] = solidPalettedFrame(t, image.Rect(0, 0, 1, 1), gifColRed, pal)
		disposal[i] = gif.DisposalNone
	}
	raw := encodeTestGIF(t, frames, delays, disposal, 0, 5000, 5000)

	_, err := safeDecodeGIF(bytes.NewReader(raw))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pixel")
}

func TestSafeDecodeGIF_RejectsOversizedScreenDimension(t *testing.T) {
	// Logical screen width over maxDecodeDim is rejected at the DecodeConfig
	// pre-check, before gif.DecodeAll ever runs.
	pal := gifTestPalette()
	frames := []*image.Paletted{solidPalettedFrame(t, image.Rect(0, 0, 1, 1), gifColRed, pal)}
	raw := encodeTestGIF(t, frames, []int{0}, []byte{gif.DisposalNone}, 0, maxDecodeDim+1, 1)

	_, err := safeDecodeGIF(bytes.NewReader(raw))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dimensions too large")
}

// The pre-decode container scan (scanGIFFrameBudget) must reject an over-budget
// animation from the DECLARED frame-descriptor dimensions alone — WITHOUT
// LZW-decoding any pixels. gifWithFakeFrameDescriptors hand-builds a tiny byte
// stream whose image descriptors declare huge frame dims (the ~1450×
// amplification shape: bytes stay tiny, DecodeAll would allocate GiB). The scan
// rejects on the summed descriptor area, never touching the (empty) pixel data.
func TestScanGIFFrameBudget_RejectsFrameDescriptorPixelBombWithoutDecode(t *testing.T) {
	const dim = 5000
	// 9 × 5000×5000 = 225,000,000 px > maxGifTotalPixels (200,000,000), under the
	// frame-count cap so the pixel arm fires.
	raw := gifWithFakeFrameDescriptors(dim, dim, 9)
	require.Less(t, len(raw), 1000, "fixture is tiny; the bomb is in declared descriptor dims, not bytes")

	err := scanGIFFrameBudget(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pixel")
}

// scanGIFFrameBudget rejects a frame-count-over-limit animation before decode.
// Directly exercises the pre-scan so the assertion is unambiguously pre-decode.
func TestScanGIFFrameBudget_RejectsFrameCountBeforeDecode(t *testing.T) {
	pal := gifTestPalette()
	n := maxGifFrames + 5
	frames := make([]*image.Paletted, n)
	delays := make([]int, n)
	disposal := make([]byte, n)
	for i := range frames {
		frames[i] = solidPalettedFrame(t, image.Rect(0, 0, 1, 1), gifColRed, pal)
		disposal[i] = gif.DisposalNone
	}
	raw := encodeTestGIF(t, frames, delays, disposal, 0, 1, 1)

	err := scanGIFFrameBudget(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frame count")
}

// scanGIFFrameBudget must fail closed on a truncated block stream.
func TestScanGIFFrameBudget_RejectsTruncatedBlockStream(t *testing.T) {
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 20, 20), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 20, 20), gifColBlue, pal),
	}
	raw := encodeTestGIF(t, frames, []int{5, 5}, []byte{gif.DisposalNone, gif.DisposalNone}, 0, 20, 20)

	// Cut mid-stream (past the header/LSD, into the descriptor/sub-block chain).
	err := scanGIFFrameBudget(raw[:len(raw)-4])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed gif")
}

// A valid small animated GIF passes the pre-scan cleanly.
func TestScanGIFFrameBudget_AcceptsValidAnimation(t *testing.T) {
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 16, 16), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 16, 16), gifColBlue, pal),
	}
	raw := encodeTestGIF(t, frames, []int{5, 5}, []byte{gif.DisposalNone, gif.DisposalNone}, 0, 16, 16)

	require.NoError(t, scanGIFFrameBudget(raw))
}

func TestSafeDecodeGIF_RejectsTruncatedData(t *testing.T) {
	pal := gifTestPalette()
	frames := []*image.Paletted{
		solidPalettedFrame(t, image.Rect(0, 0, 50, 50), gifColRed, pal),
		solidPalettedFrame(t, image.Rect(0, 0, 50, 50), gifColBlue, pal),
	}
	raw := encodeTestGIF(t, frames, []int{5, 5}, []byte{gif.DisposalNone, gif.DisposalNone}, 0, 50, 50)

	_, err := safeDecodeGIF(bytes.NewReader(raw[:len(raw)/2]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "media:")
}

func TestSafeDecodeGIF_RejectsNonGIFData(t *testing.T) {
	_, err := safeDecodeGIF(bytes.NewReader([]byte("definitely not a gif")))
	require.Error(t, err)
}

func TestSafeDecodeGIF_RejectsEmptyData(t *testing.T) {
	_, err := safeDecodeGIF(bytes.NewReader(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty image data")
}

func TestSafeDecodeGIF_ReadError(t *testing.T) {
	_, err := safeDecodeGIF(&errReader{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read")
}
