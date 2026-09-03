package desktopgateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	maxScreenshotRawBytes   = 16 << 20
	maxFramebufferDimension = 4096
	maxFramebufferPixels    = 4096 * 2160
)

var (
	errInvalidScreenshotQuery = errors.New("invalid screenshot query")
	errScreenshotCannotFit    = errors.New("screenshot cannot fit the requested byte cap")
	errScreenshotTooLarge     = errors.New("screenshot exceeds gateway processing limits")
)

// screenshot is one transformed framebuffer capture plus the geometry the
// caller needs to map encoded pixels back onto the guest's screen.
type screenshot struct {
	body          []byte
	contentType   string
	frameWidth    int
	frameHeight   int
	encodedWidth  int
	encodedHeight int
}

// transformScreenshot re-encodes a KubeVirt or Guest Desktop Agent PNG into
// the format the caller asked for. The JPEG path exists so a model can be
// handed a bounded observation: it shrinks and then degrades quality until the
// result fits maxBytes, and reports the original framebuffer geometry
// separately so click coordinates stay convertible.
func transformScreenshot(ctx context.Context, raw []byte, query url.Values) (screenshot, error) {
	if err := ctx.Err(); err != nil {
		return screenshot{}, err
	}
	if len(raw) > maxScreenshotRawBytes {
		return screenshot{}, fmt.Errorf("%w: raw PNG is %d bytes", errScreenshotTooLarge, len(raw))
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return screenshot{}, fmt.Errorf("decode KubeVirt PNG metadata: %w", err)
	}
	if configuration.Width <= 0 || configuration.Height <= 0 ||
		configuration.Width > maxFramebufferDimension || configuration.Height > maxFramebufferDimension ||
		configuration.Width > maxFramebufferPixels/configuration.Height {
		return screenshot{}, fmt.Errorf(
			"%w: framebuffer is %dx%d",
			errScreenshotTooLarge,
			configuration.Width,
			configuration.Height,
		)
	}
	if err := ctx.Err(); err != nil {
		return screenshot{}, err
	}
	if query.Get("format") == "" || query.Get("format") == "png" {
		return screenshot{
			body:          raw,
			contentType:   "image/png",
			frameWidth:    configuration.Width,
			frameHeight:   configuration.Height,
			encodedWidth:  configuration.Width,
			encodedHeight: configuration.Height,
		}, nil
	}
	if query.Get("format") != "jpeg" {
		return screenshot{}, fmt.Errorf("%w: format must be png or jpeg", errInvalidScreenshotQuery)
	}

	maxWidth, err := boundedInt(query.Get("maxWidth"), 1024, 320, 1600, "maxWidth")
	if err != nil {
		return screenshot{}, err
	}
	quality, err := boundedInt(query.Get("quality"), 65, 30, 85, "quality")
	if err != nil {
		return screenshot{}, err
	}
	maxBytes, err := boundedInt(query.Get("maxBytes"), 180_000, 50_000, 500_000, "maxBytes")
	if err != nil {
		return screenshot{}, err
	}

	source, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return screenshot{}, fmt.Errorf("decode KubeVirt PNG: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return screenshot{}, err
	}
	width := configuration.Width
	height := configuration.Height
	if width > maxWidth {
		height = max(1, height*maxWidth/width)
		width = maxWidth
	}

	for {
		if err := ctx.Err(); err != nil {
			return screenshot{}, err
		}
		resized, err := nearestNeighbor(ctx, source, width, height)
		if err != nil {
			return screenshot{}, err
		}
		for {
			if err := ctx.Err(); err != nil {
				return screenshot{}, err
			}
			var output bytes.Buffer
			if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: quality}); err != nil {
				return screenshot{}, fmt.Errorf("encode JPEG: %w", err)
			}
			if err := ctx.Err(); err != nil {
				return screenshot{}, err
			}
			if output.Len() <= maxBytes {
				return screenshot{
					body:          output.Bytes(),
					contentType:   "image/jpeg",
					frameWidth:    configuration.Width,
					frameHeight:   configuration.Height,
					encodedWidth:  width,
					encodedHeight: height,
				}, nil
			}
			if quality <= 35 {
				break
			}
			quality -= 10
		}
		if width <= 320 {
			return screenshot{}, fmt.Errorf("%w: maxBytes=%d", errScreenshotCannotFit, maxBytes)
		}
		width = max(320, width*4/5)
		height = max(1, configuration.Height*width/configuration.Width)
		quality = 55
	}
}

func boundedInt(raw string, fallback, minimum, maximum int, name string) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%w: %s must be between %d and %d", errInvalidScreenshotQuery, name, minimum, maximum)
	}
	return value, nil
}

// nearestNeighbor resizes without allocating an intermediate pyramid and
// checks the deadline every sixteen rows, so a wedged request cannot burn a
// screenshot admission slot for the length of a full resize.
func nearestNeighbor(ctx context.Context, source image.Image, width, height int) (*image.RGBA, error) {
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := source.Bounds()
	for y := 0; y < height; y++ {
		if y%16 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		sourceY := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/width
			destination.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return destination, nil
}

// kubeVirtReadStatus separates "retry later" from "this is broken" for plain
// status reads.
func kubeVirtReadStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

// screenshotReadStatus reports a missing or changed VMI as a conflict, because
// the caller's next move is to re-observe rather than to retry the same frame.
func screenshotReadStatus(err error) int {
	switch {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsTooManyRequests(err):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func screenshotTransformStatus(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable
	case errors.Is(err, errInvalidScreenshotQuery):
		return http.StatusBadRequest
	case errors.Is(err, errScreenshotCannotFit):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadGateway
	}
}
