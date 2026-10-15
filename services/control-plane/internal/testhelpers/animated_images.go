package testhelpers

import (
	"bytes"
	"compress/lzw"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// Tiny image fixtures for the inline data-URL tests (#3856). Every builder
// returns the smallest byte layout the control plane's animation detector
// reads, so a test can tell an animated image from a static one without a
// real encoder for APNG or animated WebP.

// DataURL renders raw bytes as a base64 data URL with the given declared MIME
// type. The declared type is deliberately a free parameter: the detector under
// test must key on the bytes, not on this label.
func DataURL(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// StaticPNGBytes returns a valid 1x1 PNG.
func StaticPNGBytes(tb testing.TB) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 200, G: 30, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatalf("encode static png: %v", err)
	}
	return buf.Bytes()
}

// StaticJPEGBytes returns a valid 2x2 JPEG.
func StaticJPEGBytes(tb testing.TB) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		tb.Fatalf("encode static jpeg: %v", err)
	}
	return buf.Bytes()
}

// GIFBytes returns a valid GIF with the given number of 2x2 frames; one frame
// is a static GIF and more than one animates.
func GIFBytes(tb testing.TB, frames int) []byte {
	tb.Helper()
	palette := color.Palette{color.Black, color.White}
	anim := &gif.GIF{}
	for i := 0; i < frames; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
		frame.SetColorIndex(i%2, 0, 1)
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		tb.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// APNGBytes returns a PNG; when animated is true an acTL (animation control)
// chunk sits between IHDR and IDAT, which is what makes a PNG an APNG.
func APNGBytes(tb testing.TB, animated bool) []byte {
	tb.Helper()
	base := StaticPNGBytes(tb)
	if !animated {
		return base
	}
	const sigAndIHDR = 8 + 4 + 4 + 13 + 4 // signature + length + "IHDR" + data + CRC
	actl := make([]byte, 8)
	binary.BigEndian.PutUint32(actl[0:4], 2) // num_frames
	binary.BigEndian.PutUint32(actl[4:8], 0) // num_plays (0 = infinite)
	out := append([]byte{}, base[:sigAndIHDR]...)
	out = append(out, pngChunk("acTL", actl)...)
	return append(out, base[sigAndIHDR:]...)
}

// pngChunk assembles one PNG chunk: length, type, data, CRC32(type+data).
func pngChunk(typ string, data []byte) []byte {
	out := make([]byte, 0, 12+len(data))
	out = binary.BigEndian.AppendUint32(out, uint32(len(data))) // #nosec G115 -- fixture chunks are a few bytes
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

// WebPBytes returns a RIFF/WEBP container. Animated carries a VP8X header
// whose animation flag is set, an ANIM chunk and an ANMF frame; static carries
// a VP8X header with no animation flag and a VP8L image chunk. Only the chunk
// structure is meaningful — the pixel payloads are not decodable images.
func WebPBytes(animated bool) []byte {
	vp8x := make([]byte, 10)
	if animated {
		vp8x[0] = 0x02 // animation flag
	}
	chunks := riffChunk("VP8X", vp8x)
	if animated {
		chunks = append(chunks, riffChunk("ANIM", make([]byte, 6))...)
		chunks = append(chunks, riffChunk("ANMF", make([]byte, 16))...)
	} else {
		chunks = append(chunks, riffChunk("VP8L", []byte{0x2f, 0, 0, 0, 0, 0})...)
	}
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(4+len(chunks))) // #nosec G115 -- fixture chunks are a few bytes
	out = append(out, "WEBP"...)
	return append(out, chunks...)
}

// riffChunk assembles one RIFF chunk: fourcc, little-endian size, payload, and
// a pad byte when the payload length is odd.
func riffChunk(fourcc string, payload []byte) []byte {
	out := append([]byte(fourcc), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(payload))) // #nosec G115 -- fixture payloads are a few bytes
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// GIFWithFrameHiddenFromGoDecoder returns a two-frame 2x2 GIF that browsers
// animate but Go's image/gif decodes as one frame (the #3874 red-team finding).
// Go reads a Plain Text Extension as a fixed 13 bytes before its sub-blocks;
// browsers read the byte after the label as a sub-block length. An empty one
// (21 01 00) is a no-op to a browser, but Go swallows the next 12 bytes: the
// second frame's descriptor, LZW minimum code size and first sub-block length.
// A classifier that counts frames by decoding calls this static.
func GIFWithFrameHiddenFromGoDecoder(tb testing.TB) []byte {
	tb.Helper()
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{2, 0, 2, 0, 0x80, 0, 0}) // 2x2 screen, 2-entry global colour table
	b.Write([]byte{0, 0, 0, 255, 255, 255}) // black, white

	// Frame 1: both parsers agree on it.
	b.Write([]byte{0x21, 0xF9, 0x04, 0x00, 50, 0x00, 0x00, 0x00}) // graphic control, 500 ms
	b.Write([]byte{0x2C, 0, 0, 0, 0, 2, 0, 2, 0, 0x00})
	b.WriteByte(2) // LZW minimum code size
	f1 := gifLZW(tb, []byte{0, 0, 0, 0})
	b.WriteByte(byte(len(f1))) // #nosec G115 -- a 2x2 frame encodes to a few bytes
	b.Write(f1)
	b.WriteByte(0)

	// The empty Plain Text Extension that puts Go out of step with a browser.
	b.Write([]byte{0x21, 0xF9, 0x04, 0x00, 50, 0x00, 0x00, 0x00})
	b.Write([]byte{0x21, 0x01, 0x00})

	// Frame 2: its first sub-block is one byte longer than its first LZW byte,
	// so Go, reading that LZW byte as a length, lands on the same terminator.
	b.Write([]byte{0x2C, 0, 0, 0, 0, 2, 0, 2, 0, 0x00})
	b.WriteByte(2)
	f2 := gifLZW(tb, []byte{1, 1, 1, 1})
	first := int(f2[0]) + 1
	if len(f2) > first || first > 255 {
		tb.Fatalf("gif fixture: LZW output does not fit the smuggling layout")
	}
	b.WriteByte(byte(first)) // #nosec G115 -- bounded to 255 just above
	b.Write(f2)
	b.Write(make([]byte, first-len(f2)))
	b.WriteByte(0)    // terminator both parsers reach
	b.WriteByte(0x3B) // trailer
	return b.Bytes()
}

// gifLZW encodes palette indices as image/gif does (LSB order, 2-bit literals).
func gifLZW(tb testing.TB, pixels []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	w := lzw.NewWriter(&buf, lzw.LSB, 2)
	if _, err := w.Write(pixels); err != nil {
		tb.Fatalf("gif fixture lzw: %v", err)
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("gif fixture lzw close: %v", err)
	}
	return buf.Bytes()
}
