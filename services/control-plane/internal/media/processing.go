package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register WebP decoder for image.Decode
)

// Hard limits to prevent decompression bomb attacks.
// A crafted image can be tiny on disk but expand to enormous dimensions in memory.
const (
	maxDecodePixels = 50_000_000 // 50 megapixels — absolute max before decode
	maxDecodeDim    = 10_000     // 10,000 px per side
)

// Animated-GIF decompression-bomb guards (#1302). A GIF multiplies the static
// bomb surface by its frame count, so the animated path carries two extra
// bounds on top of the per-frame maxDecodeDim/maxDecodePixels checks.
//
// The primary enforcement is a PRE-decode container scan (scanGIFFrameBudget)
// that walks the GIF byte structure — accumulating frame count and per-frame
// pixel area from the image descriptors — WITHOUT LZW-decoding any pixels. This
// is what bounds the decode work: gif.DecodeAll has no frame-count or aggregate
// cap of its own (it allocates one image.NewPaletted per descriptor in an
// unbounded loop), and DecodeConfig bounds only the logical screen (a single
// frame). Without the pre-scan, a small LZW-compressed upload with a large
// declared screen × hundreds of constant-color frames would allocate multiple
// GiB before any post-decode guard could reject it. The post-decode guards
// below are defense-in-depth; they bound the coalesce/resize loop, not the
// decode allocation.
const (
	maxGifFrames = 120 // hard frame-count ceiling for the animated pipeline

	// maxGifTotalPixels bounds the aggregate decoded paletted frame storage and
	// the frames × logical-screen work performed by the coalescing loop. Keep
	// animation within the static decoder's pixel ceiling rather than multiplying
	// that ceiling by the frame count: coalescing also needs RGBA canvases and may
	// clone one for DisposalPrevious.
	maxGifTotalPixels int64 = maxDecodePixels
)

// A single near-budget GIF can hold hundreds of MiB across decoded frames,
// coalescing canvases, and output buffers. Bound the whole decode/process
// pipeline across avatar and banner routes, not just gif.DecodeAll.
var gifProcessingSlots = make(chan struct{}, 1)

func tryAcquireGIFProcessing() bool {
	select {
	case gifProcessingSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseGIFProcessing() { <-gifProcessingSlots }

// Canonical Tier 1 image MIME types, defined once so the allowlists and the
// image-type error bodies reference named constants rather than repeating the
// literals (SonarCloud S1192). mimeGIF is also the content type stored and
// served for animation-preserving output.
const (
	mimeGIF  = "image/gif"
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
	mimeWebP = "image/webp"
)

// ProcessedImage holds the result of server-side image processing.
type ProcessedImage struct {
	Data        []byte
	ContentType string
	Width       int
	Height      int
}

// imageEncoder writes an image to a buffer and returns the content type.
type imageEncoder func(buf *bytes.Buffer, img image.Image) (string, error)

// processImageWith decodes, resizes, and re-encodes an image using the provided encoder.
func processImageWith(r io.Reader, maxWidth, maxHeight int, encode imageEncoder) (*ProcessedImage, error) {
	src, err := safeDecode(r)
	if err != nil {
		return nil, err
	}

	srcBounds := src.Bounds()
	srcW := srcBounds.Dx()
	srcH := srcBounds.Dy()

	dstW, dstH := fitDimensions(srcW, srcH, maxWidth, maxHeight)

	// Skip resize if image is already within bounds
	if dstW >= srcW && dstH >= srcH {
		dstW = srcW
		dstH = srcH
	}

	// Resize using high-quality CatmullRom interpolation
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, srcBounds, draw.Over, nil)

	var buf bytes.Buffer
	contentType, err := encode(&buf, dst)
	if err != nil {
		return nil, err
	}

	return &ProcessedImage{
		Data:        buf.Bytes(),
		ContentType: contentType,
		Width:       dstW,
		Height:      dstH,
	}, nil
}

// ProcessImage resizes an image to fit within maxWidth x maxHeight (preserving aspect ratio)
// and re-encodes it as JPEG for broad compatibility and small file size.
// Only used for Tier 1 (authenticated) media where the server can see the plaintext.
func ProcessImage(r io.Reader, maxWidth, maxHeight int) (*ProcessedImage, error) {
	return processImageWith(r, maxWidth, maxHeight, func(buf *bytes.Buffer, img image.Image) (string, error) {
		if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 85}); err != nil {
			return "", fmt.Errorf("media: failed to encode processed image: %w", err)
		}
		return mimeJPEG, nil
	})
}

// ProcessImagePNG is like ProcessImage but outputs PNG (for cases needing transparency).
func ProcessImagePNG(r io.Reader, maxWidth, maxHeight int) (*ProcessedImage, error) {
	return processImageWith(r, maxWidth, maxHeight, func(buf *bytes.Buffer, img image.Image) (string, error) {
		if err := png.Encode(buf, img); err != nil {
			return "", fmt.Errorf("media: failed to encode processed image as PNG: %w", err)
		}
		return mimePNG, nil
	})
}

// safeDecode validates image dimensions before full decode to prevent decompression bombs.
// A small compressed file can expand to gigabytes of memory during decode — this catches
// that by inspecting the header first via DecodeConfig.
func safeDecode(r io.Reader) (image.Image, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("media: failed to read image data: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("media: empty image data")
	}

	// Inspect dimensions without full decode
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("media: failed to decode image config: %w", err)
	}

	if err := validateDecodeDims(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}

	// Safe to fully decode now
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("media: failed to decode image: %w", err)
	}

	return src, nil
}

// validateDecodeDims applies the shared decompression-bomb dimension checks.
// Used by safeDecode (static, from the DecodeConfig header) and safeDecodeGIF
// (logical screen pre-check + per-frame defense-in-depth).
func validateDecodeDims(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("media: invalid image dimensions %dx%d", width, height)
	}
	if width > maxDecodeDim || height > maxDecodeDim {
		return fmt.Errorf("media: image dimensions too large: %dx%d (max %d per side)", width, height, maxDecodeDim)
	}
	if width*height > maxDecodePixels {
		return fmt.Errorf("media: image pixel count too large: %dx%d (%d pixels, max %d)", width, height, width*height, maxDecodePixels)
	}
	return nil
}

// safeDecodeGIF fully decodes an animated GIF with decompression-bomb guards
// (#1302): a DecodeConfig header pre-check bounds the logical screen with the
// same dimension/pixel limits as safeDecode, then a pre-decode container scan
// (scanGIFFrameBudget) bounds frame count and total (frames × frame) pixel
// budget WITHOUT LZW-decoding pixels — this is the guard that bounds the decode
// allocation. gif.DecodeAll runs only after the byte structure has passed both.
// The post-decode guards are defense-in-depth (they bound the coalesce/resize
// loop). Errors are PII-safe — they name bounds and counts only, never payload
// bytes.
func safeDecodeGIF(r io.Reader) (*gif.GIF, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("media: failed to read image data: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("media: empty image data")
	}

	// Header pre-check: the logical screen bounds every frame the decoder will
	// accept, so oversized screens are rejected before gif.DecodeAll runs.
	cfg, err := gif.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("media: failed to decode gif config: %w", err)
	}
	if err := validateDecodeDims(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}

	// Pre-decode container scan: walk the byte structure and enforce the
	// frame-count / total-pixel budgets before gif.DecodeAll materializes any
	// frames. Without this, a tiny compressed upload with many descriptors would
	// allocate multiple GiB before the post-decode guards could reject it.
	if err := scanGIFFrameBudget(raw); err != nil {
		return nil, err
	}

	g, err := gif.DecodeAll(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("media: failed to decode gif: %w", err)
	}
	if len(g.Image) == 0 {
		return nil, fmt.Errorf("media: gif contains no frames")
	}
	// Post-decode defense-in-depth: scanGIFFrameBudget already enforced these
	// budgets on the byte structure before any allocation. These re-checks bound
	// the coalesce/resize loop, not the decode work.
	if len(g.Image) > maxGifFrames {
		return nil, fmt.Errorf("media: gif frame count too large: %d frames (max %d)", len(g.Image), maxGifFrames)
	}
	if total := int64(len(g.Image)) * int64(cfg.Width) * int64(cfg.Height); total > maxGifTotalPixels {
		return nil, fmt.Errorf("media: gif total pixel count too large: %d frames x %dx%d screen = %d pixels (max %d)",
			len(g.Image), cfg.Width, cfg.Height, total, maxGifTotalPixels)
	}
	// The stdlib decoder already rejects frames outside the logical screen, but
	// re-assert per-frame dimensions against the decode caps.
	for _, frame := range g.Image {
		b := frame.Bounds()
		if err := validateDecodeDims(b.Dx(), b.Dy()); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// IsAnimatedImage reports whether raw is an image a browser would animate in an
// <img> element: a multi-frame GIF, an APNG (a PNG with an acTL chunk ahead of
// its first IDAT), or an animated WebP (a VP8X animation flag, or an ANIM/ANMF
// chunk). Detection reads the bytes and never a declared MIME type, so a GIF
// labelled image/png is still caught.
//
// It is the inline-image counterpart of the upload path's frame check: the
// server icon/banner routes accept `data:image/` values that never pass
// through the upload pipeline, and this lets them apply the same entitlement
// gate (#3856). Nothing is decoded: every format is classified by walking its
// container structure, so a small payload cannot make the server allocate frames.
//
// It is a classifier, not a validator. Formats are recognized by signature; the
// PNG, GIF and WebP containers are walked only as far as the answer needs, and a
// JPEG is static by signature alone. It fails closed on what it does read: a
// signature it does not know, or a container that is truncated or overruns its
// data, returns an error rather than "static", because an unclassifiable image
// cannot be proven not to animate. Errors name bounds and structure only, never
// payload bytes.
func IsAnimatedImage(raw []byte) (bool, error) {
	switch {
	case bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		return isAnimatedGIFStructure(raw)
	case bytes.HasPrefix(raw, pngSignature):
		return isAnimatedPNG(raw)
	case isRIFFWebP(raw):
		return isAnimatedWebP(raw)
	case bytes.HasPrefix(raw, jpegSignature):
		// JPEG has no animation in <img>; it is classified static by signature.
		return false, nil
	default:
		return false, fmt.Errorf("media: unrecognized image format")
	}
}

var (
	pngSignature  = []byte("\x89PNG\r\n\x1a\n")
	jpegSignature = []byte{0xFF, 0xD8, 0xFF}
)

// isAnimatedGIFStructure walks the GIF block list without decoding any pixels
// and calls the image animated once its second frame parses. It stops there,
// so the upload pipeline's frame and pixel budgets never apply: a 500-frame GIF
// is animated, not malformed. A truncated block, an unknown block, or a
// trailer with no image ahead of it is malformed.
func isAnimatedGIFStructure(raw []byte) (bool, error) {
	pos, err := skipGIFHeaderAndGCT(raw)
	if err != nil {
		return false, err
	}
	frames := 0
	for pos < len(raw) {
		block := raw[pos]
		pos++
		switch block {
		case gifTrailer:
			if frames == 0 {
				return false, fmt.Errorf("media: malformed gif: no image data")
			}
			return false, nil
		case gifExtension:
			// A graphic control block is 4 bytes by spec. Some browser decoders
			// read at least 4 whatever its size byte says, and this walk reads
			// the size byte, so any other size could hide a frame from one of
			// them. Chromium's decoder refuses it too.
			if pos+1 < len(raw) && raw[pos] == gifGraphicControlLabel && raw[pos+1] != 4 {
				return false, fmt.Errorf("media: malformed gif: graphic control block is not 4 bytes")
			}
			pos, err = scanGIFExtension(raw, pos)
		case gifImageDescriptor:
			// Parse the frame before counting it: a truncated second frame is
			// malformed, not animated.
			pos, _, _, err = scanGIFImageDescriptor(raw, pos)
			if frames++; err == nil && frames > 1 {
				return true, nil
			}
		default:
			return false, fmt.Errorf("media: malformed gif: unknown block 0x%02X", block)
		}
		if err != nil {
			return false, err
		}
	}
	return false, fmt.Errorf("media: malformed gif: missing trailer")
}

// isAnimatedPNG walks the PNG chunk list. acTL (animation control) before the
// first IDAT makes it an APNG; reaching IDAT first makes it static. A chunk that
// overruns the data, or an IEND ahead of any IDAT, is malformed.
func isAnimatedPNG(raw []byte) (bool, error) {
	const chunkHeaderLen, chunkCRCLen = 8, 4
	pos := int64(len(pngSignature))
	total := int64(len(raw))
	for {
		if total-pos < chunkHeaderLen {
			return false, fmt.Errorf("media: malformed png: no image data")
		}
		length := int64(binary.BigEndian.Uint32(raw[pos : pos+4]))
		// Bounds first: a chunk that overruns the data is malformed whatever its type.
		next := pos + chunkHeaderLen + length + chunkCRCLen
		if next > total {
			return false, fmt.Errorf("media: malformed png: chunk overruns data")
		}
		switch string(raw[pos+4 : pos+8]) {
		case "acTL":
			return true, nil
		case "IDAT":
			return false, nil
		case "IEND":
			return false, fmt.Errorf("media: malformed png: no image data")
		}
		pos = next
	}
}

// riffHeaderLen is "RIFF" + a 4-byte size + "WEBP".
const riffHeaderLen = 12

func isRIFFWebP(raw []byte) bool {
	return len(raw) >= riffHeaderLen && bytes.Equal(raw[0:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP"))
}

// vp8xAnimationFlag is the animation bit in the VP8X header's flags byte.
const vp8xAnimationFlag = 0x02

// isAnimatedWebP walks the RIFF chunk list of a WebP. The animation flag in the
// VP8X header, or any ANIM/ANMF chunk, makes it animated; the whole container is
// walked so a late ANIM chunk behind a static-looking header is still caught.
// A chunk that overruns the data, or a container with no chunk at all, is
// malformed.
func isAnimatedWebP(raw []byte) (bool, error) {
	const chunkHeaderLen = 8
	pos := int64(riffHeaderLen)
	total := int64(len(raw))
	animated, chunks := false, 0
	for total-pos >= chunkHeaderLen {
		size := int64(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		payload := pos + chunkHeaderLen
		if payload+size > total {
			return false, fmt.Errorf("media: malformed webp: chunk overruns data")
		}
		switch string(raw[pos : pos+4]) {
		case "VP8X":
			if size >= 1 && raw[payload]&vp8xAnimationFlag != 0 {
				animated = true
			}
		case "ANIM", "ANMF":
			animated = true
		}
		chunks++
		pos = payload + size + size&1 // chunks are padded to an even length
	}
	if chunks == 0 {
		return false, fmt.Errorf("media: malformed webp: no chunks")
	}
	return animated, nil
}

// GIF block-introducer bytes (GIF89a grammar).
const (
	gifImageDescriptor byte = 0x2C
	gifExtension       byte = 0x21
	gifTrailer         byte = 0x3B

	gifGraphicControlLabel byte = 0xF9
	gifPlainTextLabel      byte = 0x01
)

// scanGIFFrameBudget walks the GIF byte structure and enforces the frame-count
// and total-pixel budgets BEFORE gif.DecodeAll allocates any frame. It parses
// each image descriptor for its declared dimensions but never LZW-decodes pixel
// data, so a crafted upload that would expand to multiple GiB under DecodeAll is
// rejected while it is still a small buffer. Any truncation or malformation
// fails closed (returns an error). The logical-screen dimensions are read from
// the buffer's own Logical Screen Descriptor (already validated by the caller's
// DecodeConfig pre-check); only the LSD's packed byte is needed here, to skip
// the optional Global Color Table.
//
// The loop reads linearly: skip the header + optional GCT, then dispatch each
// block introducer to a per-block-type handler that returns the next position
// (and, for image descriptors, the running frame/pixel budget). The budget
// accounting and the block-walk both live in helpers so this function stays a
// flat dispatch loop.
func scanGIFFrameBudget(raw []byte) error {
	pos, err := skipGIFHeaderAndGCT(raw)
	if err != nil {
		return err
	}

	budget := gifFrameBudget{}
	for {
		if pos >= len(raw) {
			return fmt.Errorf("media: malformed gif: missing trailer")
		}
		block := raw[pos]
		pos++

		if block == gifTrailer {
			return nil
		}
		next, err := scanGIFBlock(raw, pos, block, &budget)
		if err != nil {
			return err
		}
		pos = next
	}
}

// gifFrameBudget accumulates the pre-decode frame-count and total-pixel budgets
// as scanGIFFrameBudget walks the image descriptors.
type gifFrameBudget struct {
	frameCount  int
	totalPixels int64
}

// skipGIFHeaderAndGCT advances past the 6-byte header + 7-byte Logical Screen
// Descriptor and, when present, the optional Global Color Table. It returns the
// position of the first block introducer.
func skipGIFHeaderAndGCT(raw []byte) (int, error) {
	// Header (6) + Logical Screen Descriptor (7). The 5th LSD byte's packed
	// flags carry the Global Color Table (GCT) presence + size.
	const headerLen, lsdLen = 6, 7
	pos := headerLen + lsdLen
	if pos > len(raw) {
		return 0, fmt.Errorf("media: malformed gif: truncated header")
	}

	// Optional Global Color Table: packed byte is the last byte of the LSD.
	packed := raw[headerLen+4]
	if packed&0x80 != 0 {
		gctSize := 3 * (1 << ((packed & 0x07) + 1))
		pos += gctSize
	}
	return pos, nil
}

// scanGIFBlock dispatches one non-trailer block introducer (already consumed by
// the caller, so pos points just past it) to its handler and returns the next
// position. Image descriptors also advance the running frame/pixel budget.
func scanGIFBlock(raw []byte, pos int, block byte, budget *gifFrameBudget) (int, error) {
	switch block {
	case gifExtension:
		// image/gif consumes a Plain Text Extension's size byte and 12
		// header bytes regardless of the declared size. A different size
		// lets this block walker skip image descriptors the decoder will see.
		if pos+1 < len(raw) && raw[pos] == gifPlainTextLabel && raw[pos+1] != 12 {
			return 0, fmt.Errorf("media: malformed gif: plain text extension block is not 12 bytes")
		}
		return scanGIFExtension(raw, pos)
	case gifImageDescriptor:
		return scanGIFImageFrame(raw, pos, budget)
	default:
		return 0, fmt.Errorf("media: malformed gif: unknown block 0x%02X", block)
	}
}

// scanGIFExtension skips an extension block: 1 label byte, then data sub-blocks.
func scanGIFExtension(raw []byte, pos int) (int, error) {
	if pos >= len(raw) {
		return 0, fmt.Errorf("media: malformed gif: truncated extension")
	}
	pos++ // skip the label byte
	return skipGIFSubBlocks(raw, pos)
}

// scanGIFImageFrame parses one image-descriptor frame and folds its declared
// area into budget, failing closed when either the frame-count or total-pixel
// ceiling is crossed. Errors are PII-safe — they report the running count and
// the limit, never payload bytes.
func scanGIFImageFrame(raw []byte, pos int, budget *gifFrameBudget) (int, error) {
	next, w, h, err := scanGIFImageDescriptor(raw, pos)
	if err != nil {
		return 0, err
	}
	budget.frameCount++
	if budget.frameCount > maxGifFrames {
		return 0, fmt.Errorf("media: gif frame count too large: %d frames (max %d)", budget.frameCount, maxGifFrames)
	}
	budget.totalPixels += int64(w) * int64(h)
	if budget.totalPixels > maxGifTotalPixels {
		return 0, fmt.Errorf("media: gif total pixel count too large: %d pixels (max %d)", budget.totalPixels, maxGifTotalPixels)
	}
	return next, nil
}

// scanGIFImageDescriptor parses one Image Descriptor starting at pos (just after
// the 0x2C introducer): the 9-byte descriptor, an optional Local Color Table,
// the LZW min-code-size byte, and the image data sub-blocks (skipped, not
// decoded). It returns the position after the frame's sub-block terminator plus
// the frame's declared width/height (little-endian uint16s from the descriptor).
func scanGIFImageDescriptor(raw []byte, pos int) (int, int, int, error) {
	const descriptorLen = 9
	if pos+descriptorLen > len(raw) {
		return 0, 0, 0, fmt.Errorf("media: malformed gif: truncated image descriptor")
	}
	// Descriptor bytes: left(2) top(2) width(2) height(2) packed(1), LE uint16s.
	w := int(raw[pos+4]) | int(raw[pos+5])<<8
	h := int(raw[pos+6]) | int(raw[pos+7])<<8
	packed := raw[pos+8]
	pos += descriptorLen

	// Optional Local Color Table.
	if packed&0x80 != 0 {
		lctSize := 3 * (1 << ((packed & 0x07) + 1))
		pos += lctSize
	}

	// LZW minimum-code-size byte, then image data sub-blocks.
	if pos >= len(raw) {
		return 0, 0, 0, fmt.Errorf("media: malformed gif: truncated image data")
	}
	pos++ // skip LZW min-code-size
	next, err := skipGIFSubBlocks(raw, pos)
	if err != nil {
		return 0, 0, 0, err
	}
	return next, w, h, nil
}

// skipGIFSubBlocks advances past a chain of GIF data sub-blocks starting at pos:
// each sub-block is a 1-byte length followed by that many data bytes, ending at
// a zero-length terminator. Returns the position just after the terminator, or
// an error if the stream is truncated.
func skipGIFSubBlocks(raw []byte, pos int) (int, error) {
	for {
		if pos >= len(raw) {
			return 0, fmt.Errorf("media: malformed gif: truncated sub-block")
		}
		n := int(raw[pos])
		pos++
		if n == 0 {
			return pos, nil
		}
		pos += n
		if pos > len(raw) {
			return 0, fmt.Errorf("media: malformed gif: sub-block overruns data")
		}
	}
}

// ProcessImageGIF resizes an animated GIF to fit within maxWidth x maxHeight
// (preserving aspect ratio) while preserving the animation: frame count,
// per-frame delays, and loop count survive re-encode (#1302). Frames are
// coalesced onto the logical canvas honoring disposal semantics BEFORE resize
// (optimized GIFs ship partial frames; naive per-frame resize glitches), then
// re-palettized. Output frames are full logical frames with disposal "none".
func ProcessImageGIF(r io.Reader, maxWidth, maxHeight int) (*ProcessedImage, error) {
	if !tryAcquireGIFProcessing() {
		return nil, fmt.Errorf("media: gif processing busy")
	}
	defer releaseGIFProcessing()

	g, err := safeDecodeGIF(r)
	if err != nil {
		return nil, err
	}
	return processDecodedGIF(g, maxWidth, maxHeight)
}

// processDecodedGIF is the coalesce → resize → re-palettize → re-encode
// pipeline over an already-guarded (safeDecodeGIF) animation. Split out so the
// upload handler can decode once for animation detection and reuse the result.
func processDecodedGIF(g *gif.GIF, maxWidth, maxHeight int) (*ProcessedImage, error) {
	screenW, screenH := g.Config.Width, g.Config.Height
	if screenW <= 0 || screenH <= 0 {
		b := g.Image[0].Bounds()
		screenW, screenH = b.Max.X, b.Max.Y
	}
	srcRect := image.Rect(0, 0, screenW, screenH)

	dstW, dstH := fitDimensions(screenW, screenH, maxWidth, maxHeight)
	// Skip resize if the animation is already within bounds (mirrors processImageWith).
	if dstW >= screenW && dstH >= screenH {
		dstW, dstH = screenW, screenH
	}
	dstRect := image.Rect(0, 0, dstW, dstH)
	needsResize := dstW != screenW || dstH != screenH

	// One consistent output palette for ALL frames. Because every output frame
	// is a fully-coalesced canvas, palettizing frame i against frame i's LOCAL
	// palette would remap earlier-frame survivor pixels to the nearest color in
	// the current local table — silent corruption for GIFs with per-frame local
	// color tables. Accumulate the union of every frame's palette instead.
	outPalette := accumulateGIFPalette(g.Image)

	out := &gif.GIF{
		Image:     make([]*image.Paletted, 0, len(g.Image)),
		Delay:     make([]int, 0, len(g.Image)),
		Disposal:  make([]byte, 0, len(g.Image)),
		LoopCount: g.LoopCount,
		Config:    image.Config{Width: dstW, Height: dstH},
	}

	canvas := image.NewRGBA(srcRect)
	for i, frame := range g.Image {
		disposal := gifFrameDisposal(g, i)
		var restore *image.RGBA
		if disposal == gif.DisposalPrevious {
			restore = cloneRGBA(canvas)
		}

		// Coalesce: composite the (possibly partial) frame onto the logical canvas.
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		out.Image = append(out.Image, renderGIFFrame(canvas, outPalette, srcRect, dstRect, needsResize))
		out.Delay = append(out.Delay, gifFrameDelay(g, i))
		// Output frames are fully coalesced, so disposal is always "none".
		out.Disposal = append(out.Disposal, gif.DisposalNone)

		canvas = applyGIFDisposal(canvas, restore, frame.Bounds(), disposal)
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return nil, fmt.Errorf("media: failed to encode processed gif: %w", err)
	}

	return &ProcessedImage{
		Data:        buf.Bytes(),
		ContentType: mimeGIF,
		Width:       dstW,
		Height:      dstH,
	}, nil
}

// gifFrameDisposal returns frame i's disposal mode, defaulting to "none" when
// the decoder produced a Disposal slice shorter than the frame list.
func gifFrameDisposal(g *gif.GIF, i int) byte {
	if i < len(g.Disposal) {
		return g.Disposal[i]
	}
	return gif.DisposalNone
}

// gifFrameDelay returns frame i's delay, defaulting to 0 when the decoder
// produced a Delay slice shorter than the frame list.
func gifFrameDelay(g *gif.GIF, i int) int {
	if i < len(g.Delay) {
		return g.Delay[i]
	}
	return 0
}

// renderGIFFrame produces one full logical output frame from the coalesced
// canvas: scale to the output size when needed (same high-quality CatmullRom
// scaler as processImageWith), then re-palettize against the accumulated
// cross-frame palette (pal) so survivor pixels from earlier frames map to a
// color that exists in the output palette.
func renderGIFFrame(canvas *image.RGBA, pal color.Palette, srcRect, dstRect image.Rectangle, needsResize bool) *image.Paletted {
	composited := image.Image(canvas)
	if needsResize {
		resized := image.NewRGBA(dstRect)
		draw.CatmullRom.Scale(resized, dstRect, canvas, srcRect, draw.Over, nil)
		composited = resized
	}

	outFrame := image.NewPaletted(dstRect, pal)
	// Error-diffusion (Floyd–Steinberg) dither onto the accumulated palette is a
	// deliberate quality choice for profile media — it hides banding when the
	// accumulated union or Plan9 fallback under-represents the source gradient.
	draw.FloydSteinberg.Draw(outFrame, dstRect, composited, image.Point{})
	return outFrame
}

// accumulateGIFPalette builds a single output palette shared by every
// re-encoded frame: the deduped union of all frames' palettes. If any source
// frame actually uses transparency, the transparent {0,0,0,0} entry is pinned
// at index 0 (so coalesced-transparent regions stay transparent); a
// fully-opaque animation is NOT charged that slot, so an animation that
// collectively uses exactly 256 distinct opaque colors keeps its real palette
// instead of overflowing to 257 and dropping to Plan9. If the union exceeds 256
// colors — the GIF palette ceiling — it falls back to a single adaptive Plan9
// palette used for ALL frames, keeping them mutually consistent (a per-frame
// local palette would corrupt earlier-frame survivor pixels).
func accumulateGIFPalette(frames []*image.Paletted) color.Palette {
	const maxPaletteColors = 256

	var acc color.Palette
	seen := map[color.RGBA64]bool{}
	if gifFramesUseTransparency(frames) {
		gifTransparent := color.RGBA{}
		acc = color.Palette{gifTransparent}
		seen[paletteKey(gifTransparent)] = true
	}

	for _, frame := range frames {
		for _, c := range frame.Palette {
			key := paletteKey(c)
			if seen[key] {
				continue
			}
			seen[key] = true
			acc = append(acc, c)
			if len(acc) > maxPaletteColors {
				return palette.Plan9
			}
		}
	}
	return acc
}

// gifFramesUseTransparency reports whether any source frame relies on
// transparency — either a fully-transparent entry in a frame's palette or a
// pixel painted with such an entry. Only then must accumulateGIFPalette reserve
// index 0 for the transparent color; an opaque animation keeps that slot for a
// real color.
func gifFramesUseTransparency(frames []*image.Paletted) bool {
	for _, frame := range frames {
		if frameUsesTransparency(frame) {
			return true
		}
	}
	return false
}

// frameUsesTransparency reports whether frame references a fully-transparent
// palette entry at any pixel.
func frameUsesTransparency(frame *image.Paletted) bool {
	transparentIdx := -1
	for i, c := range frame.Palette {
		if _, _, _, a := c.RGBA(); a == 0 {
			transparentIdx = i
			break
		}
	}
	if transparentIdx < 0 {
		return false
	}
	for _, idx := range frame.Pix {
		if int(idx) == transparentIdx {
			return true
		}
	}
	return false
}

// paletteKey normalizes a color to a comparable 16-bit-per-channel key so
// palette entries that render identically (regardless of concrete color type)
// dedupe to one accumulated-palette slot.
func paletteKey(c color.Color) color.RGBA64 {
	r, g, b, a := c.RGBA()
	// RGBA() returns alpha-premultiplied values in [0, 0xFFFF]; mask to make the
	// 16-bit narrowing provably in-range (gosec G115).
	return color.RGBA64{
		R: uint16(r & 0xFFFF),
		G: uint16(g & 0xFFFF),
		B: uint16(b & 0xFFFF),
		A: uint16(a & 0xFFFF),
	}
}

// applyGIFDisposal applies the SOURCE frame's disposal to the canvas and
// returns the canvas the next frame composites onto. restore is the
// pre-composite snapshot taken for "restore to previous" disposal (nil for
// every other mode).
func applyGIFDisposal(canvas, restore *image.RGBA, frameBounds image.Rectangle, disposal byte) *image.RGBA {
	switch disposal {
	case gif.DisposalBackground:
		draw.Draw(canvas, frameBounds, image.Transparent, image.Point{}, draw.Src)
	case gif.DisposalPrevious:
		return restore
	}
	return canvas
}

// cloneRGBA snapshots an RGBA canvas (used for GIF "restore to previous" disposal).
func cloneRGBA(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Rect)
	copy(dst.Pix, src.Pix)
	return dst
}

// fitDimensions calculates the largest dimensions that fit within maxW x maxH
// while preserving the aspect ratio of srcW x srcH.
func fitDimensions(srcW, srcH, maxW, maxH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		return maxW, maxH
	}

	ratioW := float64(maxW) / float64(srcW)
	ratioH := float64(maxH) / float64(srcH)

	ratio := ratioW
	if ratioH < ratioW {
		ratio = ratioH
	}

	dstW := int(float64(srcW) * ratio)
	dstH := int(float64(srcH) * ratio)

	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	return dstW, dstH
}
