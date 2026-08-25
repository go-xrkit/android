// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"context"
	"fmt"
)

// The same surface as the Linux transport, reporting ErrUnsupported. It exists
// so an application that captures the Android screen still builds, vets and
// tests on a macOS or Windows workstation: a compositor drives one of these
// packages per platform behind one interface, and the one it is not built for
// must still compile.

// EnvSocket names the environment variable carrying the abstract socket the XR
// host listens on. It is declared here too so a build script or a test can name
// it from any platform.
const EnvSocket = "XR_ANDROID_SOCKET"

// Available reports whether this process is running under a host that speaks
// this protocol. Off Android, never.
func Available() bool { return false }

// Displays returns every display the host can see. Off Android it reports
// [ErrUnsupported].
func Displays(context.Context) ([]Display, error) { return nil, ErrUnsupported }

// DefaultDisplay returns the built-in screen. Off Android it reports
// [ErrUnsupported].
func DefaultDisplay(context.Context) (Display, error) { return Display{}, ErrUnsupported }

// Authorized reports whether the host already holds a projection token. Off
// Android, never.
func Authorized() bool { return false }

// RequestAuthorization asks the host for the screen-capture consent dialog. Off
// Android it reports [ErrUnsupported].
func RequestAuthorization(context.Context) (bool, error) { return false, ErrUnsupported }

// CaptureDisplay starts streaming a display. Off Android it reports
// [ErrUnsupported].
func CaptureDisplay(context.Context, Display, Options) (*Stream, error) {
	return nil, ErrUnsupported
}

// Stream is a running capture. Off Android none can exist, so every method
// answers as a closed one would; the type is present so a consumer's code
// still type-checks.
type Stream struct{ opts Options }

// Frame returns the most recent frame. Off Android there is none.
func (st *Stream) Frame() (Frame, bool) { return Frame{}, false }

// WaitFrame blocks for a new frame. Off Android it reports [ErrUnsupported].
func (st *Stream) WaitFrame(context.Context) (Frame, error) { return Frame{}, ErrUnsupported }

// Stats reports what the stream has seen. Off Android, nothing.
func (st *Stream) Stats() Stats { return Stats{} }

// Options returns the options in force.
func (st *Stream) Options() Options { return st.opts }

// Format returns the pixel layout of every frame this stream produces.
func (st *Stream) Format() PixelFormat { return FormatRGBA }

// Size returns the capture's frame size in pixels. Off Android, zero.
func (st *Stream) Size() (int, int) { return 0, 0 }

// Err reports why the system stopped the capture. Off Android it is always
// [ErrUnsupported].
func (st *Stream) Err() error { return ErrUnsupported }

// String renders the stream for logs.
func (st *Stream) String() string { return fmt.Sprintf("android capture (unsupported on %s)", goos) }

// Close ends the capture. Off Android there is nothing to end, and it is
// idempotent all the same.
func (st *Stream) Close() error { return nil }
