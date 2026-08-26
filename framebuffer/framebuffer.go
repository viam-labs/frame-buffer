// Package framebuffer implements a Viam camera that holds a single image and
// serves it until it is replaced.
package framebuffer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register the PNG decoder for set_image and DecodeConfig
	"sync"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
	rutils "go.viam.com/rdk/utils"

	"github.com/viam-labs/frame-buffer/internal/verb"
)

// Model is the frame-buffer camera.
var Model = resource.NewModel("viam", "frame-buffer", "camera")

func init() {
	resource.RegisterComponent(camera.API, Model,
		resource.Registration[camera.Camera, *Config]{
			Constructor: newFrameBuffer,
		},
	)
}

// Config is the frame-buffer camera configuration.
type Config struct {
	// Camera, if set, is the upstream camera the "capture" verb pulls a
	// frame from. Leave it unset for a buffer that is only ever filled by
	// the "set_image" verb.
	Camera string `json:"camera,omitempty"`
	// SourceName picks one imager out of a multi-imager upstream camera (a
	// depth camera returns colour and depth). Empty uses the first image.
	SourceName string `json:"source_name,omitempty"`
	// DelaySec is the countdown between the "capture" verb being called and
	// the frame actually being grabbed, so a subject has time to pose.
	DelaySec float64 `json:"delay_sec,omitempty"`
}

const defaultSourceName = "frame"

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	if cfg.DelaySec < 0 {
		return nil, nil, fmt.Errorf("delay_sec must be >= 0, got %g", cfg.DelaySec)
	}
	if cfg.Camera == "" {
		return nil, nil, nil
	}
	return []string{cfg.Camera}, nil, nil
}

type frame struct {
	data       []byte
	mimeType   string
	sourceName string
	width      int
	height     int
	capturedAt time.Time
}

type frameBuffer struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name     resource.Name
	logger   logging.Logger
	cfg      *Config
	upstream camera.Camera

	mu      sync.RWMutex
	latched *frame
}

func newFrameBuffer(
	_ context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (camera.Camera, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	var upstream camera.Camera
	if cfg.Camera != "" {
		upstream, err = camera.FromProvider(deps, cfg.Camera)
		if err != nil {
			return nil, fmt.Errorf("frame-buffer: get camera dep %q: %w", cfg.Camera, err)
		}
	}
	return &frameBuffer{
		name:     conf.ResourceName(),
		logger:   logger,
		cfg:      cfg,
		upstream: upstream,
	}, nil
}

func (fb *frameBuffer) Name() resource.Name {
	return fb.name
}

// Images returns the latched frame, or an error if nothing has been captured
// or pushed yet.
func (fb *frameBuffer) Images(
	_ context.Context,
	filterSourceNames []string,
	_ map[string]interface{},
) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	fb.mu.RLock()
	f := fb.latched
	fb.mu.RUnlock()

	if f == nil {
		return nil, resource.ResponseMetadata{}, errors.New(
			"frame-buffer: no image latched yet; call the \"capture\" or \"set_image\" verb first")
	}
	if len(filterSourceNames) > 0 && !contains(filterSourceNames, f.sourceName) {
		return nil, resource.ResponseMetadata{CapturedAt: f.capturedAt}, nil
	}
	named, err := camera.NamedImageFromBytes(f.data, f.sourceName, f.mimeType, data.Annotations{})
	if err != nil {
		return nil, resource.ResponseMetadata{}, fmt.Errorf("frame-buffer: %w", err)
	}
	return []camera.NamedImage{named}, resource.ResponseMetadata{CapturedAt: f.capturedAt}, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// NextPointCloud is not supported: a frame buffer holds a 2D image.
func (fb *frameBuffer) NextPointCloud(_ context.Context, _ map[string]interface{}) (pointcloud.PointCloud, error) {
	return nil, errors.New("frame-buffer: NextPointCloud is not supported")
}

func (fb *frameBuffer) Properties(_ context.Context) (camera.Properties, error) {
	return camera.Properties{
		SupportsPCD: false,
		ImageType:   camera.ColorStream,
		MimeTypes:   []string{rutils.MimeTypeJPEG, rutils.MimeTypePNG},
	}, nil
}

func (fb *frameBuffer) Geometries(_ context.Context, _ map[string]interface{}) ([]spatialmath.Geometry, error) {
	return nil, nil
}

func (fb *frameBuffer) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	switch v {
	case "capture":
		return fb.capture(ctx)
	case "set_image":
		return fb.setImage(cmd["set_image"])
	case "clear":
		return fb.clear()
	default:
		return nil, fmt.Errorf("frame-buffer: unknown verb %q; expected \"capture\", \"set_image\", or \"clear\"", v)
	}
}

// capture counts down, grabs one frame from the upstream camera, and latches
// it. The response is metadata only — read the image itself off the camera API.
func (fb *frameBuffer) capture(ctx context.Context) (map[string]interface{}, error) {
	if fb.upstream == nil {
		return nil, errors.New("frame-buffer: capture requires camera to be configured")
	}
	if fb.cfg.DelaySec > 0 {
		fb.logger.Infof("frame-buffer: capturing in %gs", fb.cfg.DelaySec)
		timer := time.NewTimer(time.Duration(fb.cfg.DelaySec * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	var filter []string
	if fb.cfg.SourceName != "" {
		filter = []string{fb.cfg.SourceName}
	}
	images, meta, err := fb.upstream.Images(ctx, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("frame-buffer: capture from %q: %w", fb.cfg.Camera, err)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("frame-buffer: camera %q returned no images", fb.cfg.Camera)
	}
	named := images[0]
	if named.MimeType() == rutils.MimeTypeRawDepth {
		return nil, fmt.Errorf(
			"frame-buffer: camera %q returned a depth image (source %q); set source_name to the colour source",
			fb.cfg.Camera, named.SourceName)
	}
	encoded, mimeType, err := encodeForStorage(ctx, named)
	if err != nil {
		return nil, err
	}
	sourceName := named.SourceName
	if sourceName == "" {
		sourceName = defaultSourceName
	}
	capturedAt := meta.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	return fb.latch(encoded, mimeType, sourceName, capturedAt)
}

// encodeForStorage keeps already-compressed frames byte-for-byte and re-encodes
// anything else as JPEG, so consumers always get bytes a standard decoder reads.
func encodeForStorage(ctx context.Context, named camera.NamedImage) ([]byte, string, error) {
	switch named.MimeType() {
	case rutils.MimeTypeJPEG, rutils.MimeTypePNG:
		raw, err := named.Bytes(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("frame-buffer: read image bytes: %w", err)
		}
		return raw, named.MimeType(), nil
	}
	img, err := named.Image(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("frame-buffer: decode captured image: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("frame-buffer: encode captured image as jpeg: %w", err)
	}
	return buf.Bytes(), rutils.MimeTypeJPEG, nil
}

const jpegQuality = 90

type setImageArgs struct {
	ImageB64   string `json:"image_b64"`
	SourceName string `json:"source_name,omitempty"`
}

func (fb *frameBuffer) setImage(payload interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("frame-buffer: marshal payload: %w", err)
	}
	var a setImageArgs
	if unmarshalErr := json.Unmarshal(raw, &a); unmarshalErr != nil {
		return nil, fmt.Errorf("frame-buffer: parse payload: %w", unmarshalErr)
	}
	if a.ImageB64 == "" {
		return nil, errors.New("frame-buffer: image_b64 is required")
	}
	decoded, err := base64.StdEncoding.DecodeString(a.ImageB64)
	if err != nil {
		return nil, fmt.Errorf("frame-buffer: decode image_b64: %w", err)
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("frame-buffer: image_b64 is not a decodable image: %w", err)
	}
	sourceName := a.SourceName
	if sourceName == "" {
		sourceName = defaultSourceName
	}
	return fb.latch(decoded, rutils.FormatStringToMimeType(format), sourceName, time.Now())
}

func (fb *frameBuffer) latch(encoded []byte, mimeType, sourceName string, capturedAt time.Time) (map[string]interface{}, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("frame-buffer: read image dimensions: %w", err)
	}
	fb.mu.Lock()
	fb.latched = &frame{
		data:       encoded,
		mimeType:   mimeType,
		sourceName: sourceName,
		width:      cfg.Width,
		height:     cfg.Height,
		capturedAt: capturedAt,
	}
	fb.mu.Unlock()
	return map[string]interface{}{
		"width":       cfg.Width,
		"height":      cfg.Height,
		"mime_type":   mimeType,
		"source_name": sourceName,
		"captured_at": capturedAt.UTC().Format(time.RFC3339Nano),
		"size_bytes":  len(encoded),
	}, nil
}

func (fb *frameBuffer) clear() (map[string]interface{}, error) {
	fb.mu.Lock()
	had := fb.latched != nil
	fb.latched = nil
	fb.mu.Unlock()
	return map[string]interface{}{"cleared": had}, nil
}

func (fb *frameBuffer) Status(_ context.Context) (map[string]interface{}, error) {
	fb.mu.RLock()
	f := fb.latched
	fb.mu.RUnlock()
	if f == nil {
		return map[string]interface{}{"state": "empty"}, nil
	}
	return map[string]interface{}{
		"state":       "latched",
		"width":       f.width,
		"height":      f.height,
		"mime_type":   f.mimeType,
		"source_name": f.sourceName,
		"captured_at": f.capturedAt.UTC().Format(time.RFC3339Nano),
	}, nil
}
