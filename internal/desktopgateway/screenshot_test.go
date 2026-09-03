package desktopgateway

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.SetRGBA(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y % 241), B: uint8((x + y) % 239), A: 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, source); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func TestJPEGTransformPreservesFramebufferGeometryAndByteCap(t *testing.T) {
	shot, err := transformScreenshot(context.Background(), encodePNG(t, 1600, 900), url.Values{
		"format":   {"jpeg"},
		"maxWidth": {"800"},
		"maxBytes": {"100000"},
		"quality":  {"70"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if shot.contentType != "image/jpeg" || shot.frameWidth != 1600 || shot.frameHeight != 900 {
		t.Fatalf("metadata = (%q, %dx%d), want image/jpeg and 1600x900", shot.contentType, shot.frameWidth, shot.frameHeight)
	}
	if shot.encodedWidth > 800 || shot.encodedHeight <= 0 || len(shot.body) > 100000 {
		t.Fatalf("encoded = %dx%d, %d bytes", shot.encodedWidth, shot.encodedHeight, len(shot.body))
	}
}

func TestScreenshotDefaultsToUntouchedPNG(t *testing.T) {
	raw := encodePNG(t, 64, 32)
	shot, err := transformScreenshot(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if shot.contentType != "image/png" || !bytes.Equal(shot.body, raw) {
		t.Fatalf("default transform = %q, %d bytes", shot.contentType, len(shot.body))
	}
	if shot.encodedWidth != 64 || shot.encodedHeight != 32 {
		t.Fatalf("encoded geometry = %dx%d", shot.encodedWidth, shot.encodedHeight)
	}
}

func TestScreenshotTransformRejectsFrameOutsideProcessingLimits(t *testing.T) {
	_, err := transformScreenshot(context.Background(), encodePNG(t, maxFramebufferDimension+1, 1), nil)
	if !errors.Is(err, errScreenshotTooLarge) {
		t.Fatalf("oversized screenshot error = %v, want %v", err, errScreenshotTooLarge)
	}
}

func TestScreenshotTransformRejectsUnknownFormatsAndOutOfRangeBounds(t *testing.T) {
	raw := encodePNG(t, 64, 32)
	for _, query := range []url.Values{
		{"format": {"webp"}},
		{"format": {"jpeg"}, "maxWidth": {"10"}},
		{"format": {"jpeg"}, "quality": {"99"}},
		{"format": {"jpeg"}, "maxBytes": {"10"}},
		{"format": {"jpeg"}, "maxWidth": {"not-a-number"}},
	} {
		if _, err := transformScreenshot(context.Background(), raw, query); !errors.Is(err, errInvalidScreenshotQuery) {
			t.Fatalf("query %v error = %v, want %v", query, err, errInvalidScreenshotQuery)
		}
	}
}

func TestScreenshotResizeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := nearestNeighbor(ctx, image.NewRGBA(image.Rect(0, 0, 100, 100)), 100, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resize error = %v, want context canceled", err)
	}
	if _, err := transformScreenshot(ctx, encodePNG(t, 8, 8), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transform error = %v, want context canceled", err)
	}
}

func TestScreenshotErrorStatusSeparatesRetrySemantics(t *testing.T) {
	readCases := []struct {
		err  error
		want int
	}{
		{vmiNotFound(), http.StatusConflict},
		{apierrors.NewConflict(vmiResource, testName, errors.New("changed")), http.StatusConflict},
		{apierrors.NewServiceUnavailable("temporary"), http.StatusServiceUnavailable},
		{context.DeadlineExceeded, http.StatusServiceUnavailable},
		{errors.New("transport"), http.StatusBadGateway},
	}
	for _, test := range readCases {
		if got := screenshotReadStatus(test.err); got != test.want {
			t.Errorf("screenshotReadStatus(%v) = %d, want %d", test.err, got, test.want)
		}
	}

	transformCases := []struct {
		err  error
		want int
	}{
		{context.DeadlineExceeded, http.StatusServiceUnavailable},
		{errInvalidScreenshotQuery, http.StatusBadRequest},
		{errScreenshotCannotFit, http.StatusUnprocessableEntity},
		{errors.New("invalid upstream PNG"), http.StatusBadGateway},
	}
	for _, test := range transformCases {
		if got := screenshotTransformStatus(test.err); got != test.want {
			t.Errorf("screenshotTransformStatus(%v) = %d, want %d", test.err, got, test.want)
		}
	}

	if _, err := boundedInt("not-an-integer", 10, 1, 20, "example"); !errors.Is(err, errInvalidScreenshotQuery) {
		t.Fatalf("boundedInt invalid input = %v, want invalid-query classification", err)
	}
	if got, err := boundedInt("", 10, 1, 20, "example"); err != nil || got != 10 {
		t.Fatalf("boundedInt fallback = (%d, %v)", got, err)
	}
}

func TestKubeVirtReadStatusSeparatesRetryableFailures(t *testing.T) {
	if got := kubeVirtReadStatus(apierrors.NewTooManyRequestsError("slow down")); got != http.StatusServiceUnavailable {
		t.Fatalf("throttled read status = %d, want 503", got)
	}
	if got := kubeVirtReadStatus(errors.New("transport")); got != http.StatusBadGateway {
		t.Fatalf("broken read status = %d, want 502", got)
	}
}

func TestScreenshotAdmissionRejectsInsteadOfQueueingPastTheCap(t *testing.T) {
	g := &Gateway{screenshotConcurrency: 2}
	if !g.tryAcquireScreenshotSlot() || !g.tryAcquireScreenshotSlot() {
		t.Fatal("screenshot admission rejected below its configured cap")
	}
	if g.tryAcquireScreenshotSlot() {
		t.Fatal("screenshot admission queued or admitted a request above its cap")
	}
	g.releaseScreenshotSlot()
	if !g.tryAcquireScreenshotSlot() {
		t.Fatal("released screenshot capacity was not reusable")
	}
	g.releaseScreenshotSlot()
	g.releaseScreenshotSlot()
}
