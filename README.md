# frame-buffer

A Viam camera that holds a **single image** and serves it until it is replaced.

Ordinary cameras stream whatever they see right now. This one latches one frame
and keeps serving it, which makes two things possible:

- **Take a photo on command.** Point it at an upstream camera, call `capture`,
  and the frame it grabbed stays put — visible in the Viam app's camera panel —
  until you take another.
- **Show a generated image in the app.** Push bytes in with `set_image` and any
  image your code produces becomes viewable in the app, without a separate
  viewer or a base64 round trip.

## Model: `viam:frame-buffer:camera`

### Configuration

```json
{
  "camera": "camera-1",
  "source_name": "color",
  "delay_sec": 3
}
```

| Attribute | Type | Required | Description |
|---|---|---|---|
| `camera` | string | no | Upstream camera the `capture` verb pulls a frame from. Omit for a buffer only ever filled by `set_image`. |
| `source_name` | string | no | Which imager to take from a multi-imager camera. A depth camera returns both colour and depth — set this to the colour source (`"color"` on a RealSense). Empty uses the first image returned. |
| `delay_sec` | number | no | Countdown between `capture` being called and the frame being grabbed, so a subject has time to pose. Defaults to 0. |

Configuring `camera` is what makes `capture` available; everything else works
without an upstream.

### `capture`

Waits out `delay_sec`, grabs one frame from the upstream camera, and latches it.

```json
{"capture": {}}
```

The response is metadata only — the image itself is served over the camera API,
so calling this from the app doesn't fill the response pane with base64:

```json
{
  "width": 1280,
  "height": 720,
  "mime_type": "image/jpeg",
  "source_name": "color",
  "captured_at": "2026-08-26T16:41:02.113Z",
  "size_bytes": 184203
}
```

Frames that arrive already compressed (JPEG or PNG) are stored byte-for-byte;
anything else is re-encoded as JPEG so consumers always get bytes a standard
decoder can read. If the upstream hands back a depth frame, `capture` fails with
a message telling you to set `source_name` rather than latching unusable data.

### `set_image`

Latches an image you supply. The bytes must decode as JPEG or PNG.

```json
{"set_image": {"image_b64": "iVBORw0KGgo…", "source_name": "line-preview"}}
```

Returns the same metadata shape as `capture`.

### `clear`

Drops the latched frame. `{"cleared": true}` if there was one.

```json
{"clear": {}}
```

## Reading the image

Anything that speaks the camera API works — the Viam app's camera panel, the
SDKs, or another module holding this camera as a dependency:

```go
images, _, err := cam.Images(ctx, nil, nil)
raw, err := images[0].Bytes(ctx)
```

Before the first `capture` or `set_image`, the buffer is empty and reads return
an error saying so. The app's camera panel shows that as a failed frame; it
starts working as soon as something is latched.

The buffer lives in memory only — a module restart empties it.

## Limitations

- One image at a time. There is no history.
- `NextPointCloud` is not supported.
- `capture` grabs a single frame on demand; it is not a recorder.
