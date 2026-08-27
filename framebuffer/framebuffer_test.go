package framebuffer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/testutils/inject"
	rutils "go.viam.com/rdk/utils"
	"go.viam.com/test"
)

func testPNGBase64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	test.That(t, png.Encode(&buf, img), test.ShouldBeNil)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func newTestBuffer(t *testing.T, cfg *Config) *frameBuffer {
	t.Helper()
	return &frameBuffer{logger: logging.NewTestLogger(t), cfg: cfg}
}

func TestConfigValidate_negativeDelay(t *testing.T) {
	_, _, err := (&Config{DelaySec: -1}).Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "delay_sec")
}

func TestConfigValidate_noCameraMeansNoDeps(t *testing.T) {
	deps, _, err := (&Config{}).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldBeEmpty)
}

func TestConfigValidate_cameraIsADep(t *testing.T) {
	deps, _, err := (&Config{Camera: "cam-1", DelaySec: 3}).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"cam-1"})
}

func TestImages_emptyBuffer(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, _, err := fb.Images(context.Background(), nil, nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "no image latched")
}

func TestSetImage_latchesAndServes(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	resp, err := fb.setImage(map[string]interface{}{"image_b64": testPNGBase64(t, 8, 4)})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["width"], test.ShouldEqual, 8)
	test.That(t, resp["height"], test.ShouldEqual, 4)
	test.That(t, resp["mime_type"], test.ShouldEqual, rutils.MimeTypePNG)
	test.That(t, resp["source_name"], test.ShouldEqual, defaultSourceName)

	images, _, err := fb.Images(context.Background(), nil, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(images), test.ShouldEqual, 1)
	test.That(t, images[0].SourceName, test.ShouldEqual, defaultSourceName)
	test.That(t, images[0].MimeType(), test.ShouldEqual, rutils.MimeTypePNG)
}

func TestSetImage_customSourceName(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.setImage(map[string]interface{}{
		"image_b64":   testPNGBase64(t, 2, 2),
		"source_name": "line-preview",
	})
	test.That(t, err, test.ShouldBeNil)
	images, _, err := fb.Images(context.Background(), []string{"line-preview"}, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(images), test.ShouldEqual, 1)
}

func TestImages_filterMissesSource(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.setImage(map[string]interface{}{"image_b64": testPNGBase64(t, 2, 2)})
	test.That(t, err, test.ShouldBeNil)
	images, _, err := fb.Images(context.Background(), []string{"nope"}, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, images, test.ShouldBeEmpty)
}

func TestSetImage_missingImage(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.setImage(map[string]interface{}{})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "image_b64")
}

func TestSetImage_notBase64(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.setImage(map[string]interface{}{"image_b64": "!!!not base64!!!"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "decode image_b64")
}

func TestSetImage_notAnImage(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.setImage(map[string]interface{}{
		"image_b64": base64.StdEncoding.EncodeToString([]byte("hello, not an image")),
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "not a decodable image")
}

func TestClear(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	resp, err := fb.clear()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["cleared"], test.ShouldEqual, false)

	_, err = fb.setImage(map[string]interface{}{"image_b64": testPNGBase64(t, 2, 2)})
	test.That(t, err, test.ShouldBeNil)
	resp, err = fb.clear()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["cleared"], test.ShouldEqual, true)
	_, _, err = fb.Images(context.Background(), nil, nil)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestCapture_noUpstream(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.capture(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "requires camera")
}

func TestDoCommand_unknownVerb(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.DoCommand(context.Background(), map[string]interface{}{"nope": map[string]interface{}{}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "unknown verb")
}

func TestDoCommand_multipleVerbs(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.DoCommand(context.Background(), map[string]interface{}{"capture": nil, "clear": nil})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "exactly one verb")
}

func TestStatus(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	status, err := fb.Status(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status["state"], test.ShouldEqual, "empty")

	_, err = fb.setImage(map[string]interface{}{"image_b64": testPNGBase64(t, 6, 3)})
	test.That(t, err, test.ShouldBeNil)
	status, err = fb.Status(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status["state"], test.ShouldEqual, "latched")
	test.That(t, status["width"], test.ShouldEqual, 6)
}

func TestNextPointCloud_unsupported(t *testing.T) {
	fb := newTestBuffer(t, &Config{})
	_, err := fb.NextPointCloud(context.Background(), nil)
	test.That(t, err, test.ShouldNotBeNil)
}

func depthBytes(w, h int) []byte {
	buf := new(bytes.Buffer)
	buf.WriteString("DEPTHMAP")
	_ = binary.Write(buf, binary.BigEndian, uint64(w))
	_ = binary.Write(buf, binary.BigEndian, uint64(h))
	for i := 0; i < w*h; i++ {
		_ = binary.Write(buf, binary.BigEndian, uint16(1000+i))
	}
	return buf.Bytes()
}

func upstreamWith(t *testing.T, names ...string) *inject.Camera {
	t.Helper()
	cam := inject.NewCamera("cam-1")
	cam.ImagesFunc = func(_ context.Context, filter []string, _ map[string]interface{},
	) ([]camera.NamedImage, resource.ResponseMetadata, error) {
		var out []camera.NamedImage
		for _, n := range names {
			if len(filter) > 0 && !contains(filter, n) {
				continue
			}
			var (
				raw  []byte
				mime string
			)
			if n == "depth" {
				raw, mime = depthBytes(4, 3), rutils.MimeTypeRawDepth
			} else {
				decoded, _ := base64.StdEncoding.DecodeString(testPNGBase64(t, 4, 3))
				raw, mime = decoded, rutils.MimeTypePNG
			}
			img, err := camera.NamedImageFromBytes(raw, n, mime, data.Annotations{})
			test.That(t, err, test.ShouldBeNil)
			out = append(out, img)
		}
		return out, resource.ResponseMetadata{CapturedAt: time.Unix(1700000000, 0)}, nil
	}
	return cam
}

func TestCapture_latchesEverySource(t *testing.T) {
	fb := newTestBuffer(t, &Config{Camera: "cam-1"})
	fb.upstream = upstreamWith(t, "color", "depth")
	resp, err := fb.capture(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["source_name"], test.ShouldEqual, "color")
	test.That(t, len(resp["sources"].([]interface{})), test.ShouldEqual, 2)

	images, _, err := fb.Images(context.Background(), nil, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(images), test.ShouldEqual, 2)
}

func TestCapture_depthIsStoredVerbatim(t *testing.T) {
	fb := newTestBuffer(t, &Config{Camera: "cam-1"})
	fb.upstream = upstreamWith(t, "color", "depth")
	_, err := fb.capture(context.Background())
	test.That(t, err, test.ShouldBeNil)

	images, _, err := fb.Images(context.Background(), []string{"depth"}, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(images), test.ShouldEqual, 1)
	test.That(t, images[0].MimeType(), test.ShouldEqual, rutils.MimeTypeRawDepth)
	raw, err := images[0].Bytes(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, raw, test.ShouldResemble, depthBytes(4, 3))
}

func TestCapture_renderableSourceComesFirst(t *testing.T) {
	fb := newTestBuffer(t, &Config{Camera: "cam-1"})
	fb.upstream = upstreamWith(t, "depth", "color")
	resp, err := fb.capture(context.Background())
	test.That(t, err, test.ShouldBeNil)
	// A viewer asking for "the image" must not be handed a depth map.
	test.That(t, resp["source_name"], test.ShouldEqual, "color")
	images, _, err := fb.Images(context.Background(), nil, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, images[0].SourceName, test.ShouldEqual, "color")
}

func TestCapture_singleSourceUnchanged(t *testing.T) {
	fb := newTestBuffer(t, &Config{Camera: "cam-1"})
	fb.upstream = upstreamWith(t, "color")
	resp, err := fb.capture(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["width"], test.ShouldEqual, 4)
	test.That(t, resp["height"], test.ShouldEqual, 3)
	test.That(t, len(resp["sources"].([]interface{})), test.ShouldEqual, 1)
}

func TestStatus_listsEverySource(t *testing.T) {
	fb := newTestBuffer(t, &Config{Camera: "cam-1"})
	fb.upstream = upstreamWith(t, "color", "depth")
	_, err := fb.capture(context.Background())
	test.That(t, err, test.ShouldBeNil)
	status, err := fb.Status(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status["state"], test.ShouldEqual, "latched")
	test.That(t, len(status["sources"].([]interface{})), test.ShouldEqual, 2)
}
