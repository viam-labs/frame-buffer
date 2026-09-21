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
	"sort"
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

func (f *frame) isDepth() bool {
	return f.mimeType == rutils.MimeTypeRawDepth
}

func (f *frame) describe() map[string]interface{} {
	return map[string]interface{}{
		"source_name": f.sourceName,
		"mime_type":   f.mimeType,
		"width":       f.width,
		"height":      f.height,
		"size_bytes":  len(f.data),
	}
}

// newFrame stores one image as it arrived. Depth is kept verbatim: it is not a
// picture, and re-encoding it as JPEG would destroy the millimetre values that
// are the only reason to carry it.
func newFrame(ctx context.Context, named camera.NamedImage, capturedAt time.Time) (*frame, error) {
	sourceName := named.SourceName
	if sourceName == "" {
		sourceName = defaultSourceName
	}
	encoded, mimeType, err := encodeForStorage(ctx, named)
	if err != nil {
		return nil, err
	}
	f := &frame{
		data:       encoded,
		mimeType:   mimeType,
		sourceName: sourceName,
		capturedAt: capturedAt,
	}
	if cfg, _, cfgErr := image.DecodeConfig(bytes.NewReader(encoded)); cfgErr == nil {
		f.width, f.height = cfg.Width, cfg.Height
	}
	return f, nil
}

type frameBuffer struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name     resource.Name
	logger   logging.Logger
	cfg      *Config
	upstream camera.Camera

	mu sync.RWMutex
	// latched holds every image the upstream returned, in its order. A depth
	// camera returns colour and depth together, and a consumer that wants to
	// segment on depth needs the pair from the same instant — latching only the
	// first would make that impossible to reconstruct later.
	latched []*frame
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
	frames := fb.latched
	fb.mu.RUnlock()

	if len(frames) == 0 {
		return nil, resource.ResponseMetadata{}, errors.New(
			"frame-buffer: no image latched yet; call the \"capture\" or \"set_image\" verb first",
		)
	}
	meta := resource.ResponseMetadata{CapturedAt: frames[0].capturedAt}
	out := make([]camera.NamedImage, 0, len(frames))
	for _, f := range frames {
		if len(filterSourceNames) > 0 && !contains(filterSourceNames, f.sourceName) {
			continue
		}
		named, err := camera.NamedImageFromBytes(f.data, f.sourceName, f.mimeType, data.Annotations{})
		if err != nil {
			return nil, resource.ResponseMetadata{}, fmt.Errorf("frame-buffer: %w", err)
		}
		out = append(out, named)
	}
	return out, meta, nil
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

// capture counts down, grabs the upstream camera's frames, and latches them.
// The response is metadata only — read the images off the camera API.
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
	capturedAt := meta.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}

	frames := make([]*frame, 0, len(images))
	for i := range images {
		f, err := newFrame(ctx, images[i], capturedAt)
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	}
	// Renderable images first, so a viewer asking for "the image" gets one it
	// can display rather than a depth map.
	sort.SliceStable(frames, func(i, j int) bool {
		return !frames[i].isDepth() && frames[j].isDepth()
	})
	return fb.latchAll(frames)
}

// encodeForStorage keeps already-compressed frames byte-for-byte and re-encodes
// anything else as JPEG, so consumers always get bytes a standard decoder reads.
func encodeForStorage(ctx context.Context, named camera.NamedImage) ([]byte, string, error) {
	switch named.MimeType() {
	case rutils.MimeTypeRawDepth, rutils.MimeTypeJPEG, rutils.MimeTypePNG:
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

func (fb *frameBuffer) latchAll(frames []*frame) (map[string]interface{}, error) {
	if len(frames) == 0 {
		return nil, errors.New("frame-buffer: nothing to latch")
	}
	fb.mu.Lock()
	fb.latched = frames
	fb.mu.Unlock()
	return describeSet(frames), nil
}

func (fb *frameBuffer) latch(encoded []byte, mimeType, sourceName string, capturedAt time.Time) (map[string]interface{}, error) {
	f := &frame{
		data:       encoded,
		mimeType:   mimeType,
		sourceName: sourceName,
		capturedAt: capturedAt,
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(encoded)); err == nil {
		f.width, f.height = cfg.Width, cfg.Height
	} else if !f.isDepth() {
		return nil, fmt.Errorf("frame-buffer: read image dimensions: %w", err)
	}
	return fb.latchAll([]*frame{f})
}

// describeSet reports the first frame's details at the top level so a single
// image reads the way it always has, with every source listed alongside.
func describeSet(frames []*frame) map[string]interface{} {
	sources := make([]interface{}, 0, len(frames))
	for _, f := range frames {
		sources = append(sources, f.describe())
	}
	first := frames[0]
	return map[string]interface{}{
		"width":       first.width,
		"height":      first.height,
		"mime_type":   first.mimeType,
		"source_name": first.sourceName,
		"captured_at": first.capturedAt.UTC().Format(time.RFC3339Nano),
		"size_bytes":  len(first.data),
		"sources":     sources,
	}
}

func (fb *frameBuffer) clear() (map[string]interface{}, error) {
	fb.mu.Lock()
	had := len(fb.latched) > 0
	fb.latched = nil
	fb.mu.Unlock()
	return map[string]interface{}{"cleared": had}, nil
}

func (fb *frameBuffer) Status(_ context.Context) (map[string]interface{}, error) {
	fb.mu.RLock()
	frames := fb.latched
	fb.mu.RUnlock()
	if len(frames) == 0 {
		return map[string]interface{}{"state": "empty"}, nil
	}
	out := describeSet(frames)
	delete(out, "size_bytes")
	out["state"] = "latched"
	return out, nil
}
