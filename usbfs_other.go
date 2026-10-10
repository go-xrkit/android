// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import "context"

// USBHandle is one USB device this application was given permission to open.
// Off Android there is none, and every method reports [ErrUnsupported].
type USBHandle struct{ name string }

// OpenUSB asks the host to open a USB device. Off Android there is no host to
// ask, so it reports [ErrUnsupported] — after the portable check that a device
// with no name cannot be opened, which behaves identically everywhere.
func OpenUSB(_ context.Context, d USBDevice) (*USBHandle, error) {
	if d.Name == "" {
		return nil, ErrInvalidOption
	}
	return nil, ErrUnsupported
}

// Name is the device node this handle was opened for.
func (h *USBHandle) Name() string { return h.name }

// Descriptors reads the device's raw descriptor blob.
func (h *USBHandle) Descriptors() (USBDescriptors, error) { return nil, ErrUnsupported }

// ClaimInterface takes an interface from whatever kernel driver holds it.
func (h *USBHandle) ClaimInterface(int) error { return ErrUnsupported }

// SetAltSetting selects an alternate setting of an interface.
func (h *USBHandle) SetAltSetting(int, int) error { return ErrUnsupported }

// SubmitISO queues one isochronous request against an endpoint.
func (h *USBHandle) SubmitISO(USBEndpoint) error { return ErrUnsupported }

// Close releases everything the handle holds. It is idempotent.
func (h *USBHandle) Close() error { return nil }
