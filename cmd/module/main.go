// Package main runs the frame-buffer Viam module.
package main

import (
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	"github.com/viam-labs/frame-buffer/framebuffer"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: camera.API, Model: framebuffer.Model},
	)
}
