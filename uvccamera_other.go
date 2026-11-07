// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"context"
	"errors"
	"image"
)

// UVCCamera is the shape the linux build fills in. Nothing opens one here.
//
// ⛔ THE FIELDS ARE PART OF THE SHAPE, and leaving them out is how a caller
// compiles on one platform only: `cam.Size` is a compile error here and not
// there. Measured on the first build of this file — cmd/xrhead, in the same
// repository, stopped building for darwin while android was green.
type UVCCamera struct {
	// Device is the camera this opened. Zero: nothing was opened.
	Device USBDevice
	// Format and Size are what the camera agreed to. Zero here.
	Format UVCFormatDesc
	Size   UVCFrameSize
	// Negotiation is the committed probe/commit result. Zero here.
	Negotiation UVCNegotiation
}

// ErrNoCamera says no UVC camera was attached.
var ErrNoCamera = errors.New("android: no UVC camera is attached")

// OpenUVCCamera refuses away from Linux: usbfs is the only route to these
// frames, and it is a Linux interface. It is declared here so a caller compiles
// everywhere and discovers the platform at RUN time, which is where a caller
// can say something useful about it.
func OpenUVCCamera(context.Context, UVCCameraOptions, func(string, ...any)) (*UVCCamera, error) {
	return nil, ErrUnsupported
}

// Frame has no camera to read.
func (c *UVCCamera) Frame(context.Context) (*image.RGBA, error) { return nil, ErrUnsupported }

// Stream has none.
func (c *UVCCamera) Stream() *UVCStream { return nil }

// Detached took nothing from anybody.
func (c *UVCCamera) Detached() []int { return nil }

// Close has nothing open.
func (c *UVCCamera) Close() error { return nil }
