package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/gif"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tier-1 metadata-strip regression tests (#2469).
//
// Tier-1 media — avatars, banners, server icons, group-DM icons — is stored
// server-readable, and every upload is fully decoded and re-encoded through Go's
// standard-library encoders, which write no EXIF, XMP, or IPTC segment. So
// metadata removal already happens.
//
// It happens as a SIDE EFFECT of resizing, and nothing asserted it. A future
// "skip the re-encode when the image is already within bounds" optimisation
// would silently reintroduce the leak with no test failing, and the privacy
// policy now states publicly that this metadata is removed (#2847). These tests
// turn an emergent property into a pinned invariant.
//
// The fixtures splice a real APP1/EXIF block carrying a recognisable GPS payload
// into a valid JPEG. Each test asserts the marker is PRESENT before processing
// and ABSENT after: without the pre-assertion, a fixture bug produces an image
// that never carried GPS, and the removal assertion passes proving nothing.

// gpsMarker is a distinctive byte run embedded in the synthesized EXIF payload.
// It is not a real GPS encoding — it only has to be findable and absent-able.
var gpsMarker = []byte{0x47, 0x50, 0x53, 0xDE, 0xAD, 0xBE, 0xEF}

// exifAPP1 builds an APP1 segment whose payload is "Exif\0\0" followed by a
// minimal big-endian TIFF header and the GPS marker. Real decoders would find
// it malformed past the header; that is irrelevant here, because the property
// under test is that the bytes do not survive a re-encode.
func exifAPP1(t *testing.T) []byte {
	t.Helper()
	header := []byte{'E', 'x', 'i', 'f', 0x00, 0x00, 'M', 'M', 0x00, 0x2A, 0x00, 0x00, 0x00, 0x08}
	payload := make([]byte, 0, len(header)+len(gpsMarker))
	payload = append(payload, header...)
	payload = append(payload, gpsMarker...)

	// A JPEG segment's length field covers itself and is a uint16. The payload
	// here is a fixed 21 bytes, so this cannot overflow — asserted rather than
	// assumed so the conversion below is provably in range.
	length := len(payload) + 2
	if length > math.MaxUint16 {
		t.Fatalf("APP1 payload of %d bytes exceeds the JPEG segment limit", length)
	}

	segment := make([]byte, 4, 4+len(payload))
	segment[0], segment[1] = 0xFF, 0xE1
	//nolint:gosec // G115: bounded by the explicit MaxUint16 guard directly above.
	binary.BigEndian.PutUint16(segment[2:4], uint16(length))
	return append(segment, payload...)
}

// jpegWithEXIF splices an EXIF APP1 in directly after SOI, which is where a
// camera writes it.
func jpegWithEXIF(t *testing.T, w, h int) []byte {
	t.Helper()
	base := createTestJPEG(t, w, h).Bytes()
	require.GreaterOrEqual(t, len(base), 2, "encoded JPEG is impossibly short")

	out := make([]byte, 0, len(base)+64)
	out = append(out, base[0], base[1]) // SOI
	out = append(out, exifAPP1(t)...)
	return append(out, base[2:]...)
}

func TestProcessImageStripsEXIF(t *testing.T) {
	src := jpegWithEXIF(t, 200, 150)
	require.True(t, bytes.Contains(src, gpsMarker),
		"fixture must carry the GPS marker before processing, or this test proves nothing")

	processed, err := ProcessImage(bytes.NewReader(src), 100, 100)
	require.NoError(t, err)

	assert.False(t, bytes.Contains(processed.Data, gpsMarker),
		"GPS metadata survived the tier-1 re-encode")
	assert.False(t, bytes.Contains(processed.Data, []byte("Exif")),
		"an EXIF identifier survived the tier-1 re-encode")
}

func TestProcessImagePNGStripsEXIF(t *testing.T) {
	src := jpegWithEXIF(t, 200, 150)
	require.True(t, bytes.Contains(src, gpsMarker), "fixture must carry the GPS marker")

	// The PNG encoder path takes any decodable input; a JPEG source proves the
	// metadata is dropped at decode rather than merely not re-emitted.
	processed, err := ProcessImagePNG(bytes.NewReader(src), 100, 100)
	require.NoError(t, err)

	assert.False(t, bytes.Contains(processed.Data, gpsMarker), "GPS metadata survived PNG re-encode")
	assert.False(t, bytes.Contains(processed.Data, []byte("eXIf")), "a PNG eXIf chunk was emitted")
}

// TestProcessImageStripsEXIFWhenNoResizeNeeded is the important one. The resize
// is skipped when the image already fits, so this is the path a future
// "skip the re-encode for small images" optimisation would target. The strip
// must hold even when no scaling happens.
func TestProcessImageStripsEXIFWhenNoResizeNeeded(t *testing.T) {
	src := jpegWithEXIF(t, 50, 50)
	require.True(t, bytes.Contains(src, gpsMarker), "fixture must carry the GPS marker")

	processed, err := ProcessImage(bytes.NewReader(src), 500, 500) // bounds exceed the image
	require.NoError(t, err)

	assert.False(t, bytes.Contains(processed.Data, gpsMarker),
		"GPS survived when no resize was required — the strip must not depend on scaling")
}

func TestProcessImageGIFStripsComment(t *testing.T) {
	// A GIF Comment Extension: 0x21 0xFE, one length-prefixed sub-block, then a
	// zero terminator. A sub-block length is a single byte, so the marker must
	// fit in 255 — asserted rather than assumed.
	if len(gpsMarker) > math.MaxUint8 {
		t.Fatalf("GPS marker of %d bytes does not fit one GIF sub-block", len(gpsMarker))
	}
	comment := make([]byte, 0, 4+len(gpsMarker))
	//nolint:gosec // G115: bounded by the explicit MaxUint8 guard directly above.
	comment = append(comment, 0x21, 0xFE, uint8(len(gpsMarker)))
	comment = append(comment, gpsMarker...)
	comment = append(comment, 0x00)

	pal := gifTestPalette()
	rect := image.Rect(0, 0, 60, 40)
	frames := []*image.Paletted{
		solidPalettedFrame(t, rect, gifColRed, pal),
		solidPalettedFrame(t, rect, gifColBlue, pal),
	}
	base := encodeTestGIF(t, frames, []int{10, 10}, []byte{gif.DisposalNone, gif.DisposalNone}, 0, 60, 40)
	require.GreaterOrEqual(t, len(base), 13, "encoded GIF is impossibly short")

	// Anchor on the first extension introducer rather than assuming a fixed
	// offset: the header is 13 bytes but a global colour table of unknown size
	// may follow it.
	at := bytes.IndexByte(base[13:], 0x21)
	require.GreaterOrEqual(t, at, 0, "test GIF has no extension block to anchor against")
	at += 13

	src := make([]byte, 0, len(base)+len(comment))
	src = append(src, base[:at]...)
	src = append(src, comment...)
	src = append(src, base[at:]...)

	require.True(t, bytes.Contains(src, gpsMarker), "fixture must carry the GPS marker")

	processed, err := ProcessImageGIF(bytes.NewReader(src), 30, 30)
	require.NoError(t, err)

	assert.False(t, bytes.Contains(processed.Data, gpsMarker),
		"a GIF comment extension survived the animation-preserving re-encode")
}
