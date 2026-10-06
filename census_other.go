// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import "context"

// Cameras returns every camera the camera2 API can see. Off Android there is no
// host to ask, so it reports [ErrUnsupported].
func Cameras(context.Context) ([]Camera, error) { return nil, ErrUnsupported }

// USBDevices returns every device on the phone's USB host port. Off Android
// there is no host to ask, so it reports [ErrUnsupported].
func USBDevices(context.Context) ([]USBDevice, error) { return nil, ErrUnsupported }
