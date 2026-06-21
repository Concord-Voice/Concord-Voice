package media

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	_ "image/gif" // Register GIF decoder for image.Decode

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register WebP decoder for image.Decode
)

// Hard limits to prevent decompression bomb attacks.
// A crafted image can be tiny on disk but expand to enormous dimensions in memory.
const (
	maxDecodePixels = 50_000_000 // 50 megapixels — absolute max before decode
	maxDecodeDim    = 10_000     // 10,000 px per side
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
		return "image/jpeg", nil
	})
}

// ProcessImagePNG is like ProcessImage but outputs PNG (for cases needing transparency).
func ProcessImagePNG(r io.Reader, maxWidth, maxHeight int) (*ProcessedImage, error) {
	return processImageWith(r, maxWidth, maxHeight, func(buf *bytes.Buffer, img image.Image) (string, error) {
		if err := png.Encode(buf, img); err != nil {
			return "", fmt.Errorf("media: failed to encode processed image as PNG: %w", err)
		}
		return "image/png", nil
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

	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("media: invalid image dimensions %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width > maxDecodeDim || cfg.Height > maxDecodeDim {
		return nil, fmt.Errorf("media: image dimensions too large: %dx%d (max %d per side)", cfg.Width, cfg.Height, maxDecodeDim)
	}
	if cfg.Width*cfg.Height > maxDecodePixels {
		return nil, fmt.Errorf("media: image pixel count too large: %dx%d (%d pixels, max %d)", cfg.Width, cfg.Height, cfg.Width*cfg.Height, maxDecodePixels)
	}

	// Safe to fully decode now
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("media: failed to decode image: %w", err)
	}

	return src, nil
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
