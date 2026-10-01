package media

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Feedback screenshots are embedded in public GitHub issues through GitHub's
// camo proxy, which refuses images over 5,242,880 bytes; a lossless PNG of a
// 2560px photo routinely exceeds that and renders as a broken image. processImage
// must therefore route purposeFeedbackScreenshot to the JPEG encoder even for
// PNG input (#1747 review). The control arm pins that the routing is keyed on
// the purpose — an ordinary non-banner purpose keeps the PNG default — so
// reverting the branch fails the first assertion and not the second.
func TestProcessImage_FeedbackScreenshotEncodesJPEG(t *testing.T) {
	out, err := processImage(createTestPNG(t, 64, 64), purposeFeedbackScreenshot, FeedbackScreenshotMaxDim, FeedbackScreenshotMaxDim)
	require.NoError(t, err)
	require.Equal(t, mimeJPEG, out.ContentType,
		"feedback screenshots must be stored as JPEG so the served object stays under camo's cap")

	out, err = processImage(createTestPNG(t, 64, 64), purposeDMIcon, IconMaxDim, IconMaxDim)
	require.NoError(t, err)
	require.Equal(t, mimePNG, out.ContentType, "non-banner purposes other than feedback keep the PNG path")
}
