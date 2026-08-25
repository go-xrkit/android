// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Off Android there is no host, so the whole surface must answer
// ErrUnsupported rather than panic — an application that captures the Android
// screen still has to build, vet and run its own tests on a macOS or Windows
// workstation.
func TestEverythingIsUnsupportedOffAndroid(t *testing.T) {
	if Available() {
		t.Error("Available said yes off Android")
	}
	if Authorized() {
		t.Error("Authorized said yes off Android")
	}
	if _, err := Displays(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Displays = %v", err)
	}
	if _, err := DefaultDisplay(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DefaultDisplay = %v", err)
	}
	if _, err := RequestAuthorization(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("RequestAuthorization = %v", err)
	}
	if _, err := CaptureDisplay(context.Background(), Display{}, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CaptureDisplay = %v", err)
	}
}

func TestTheStubStreamAnswersAsAClosedOneWould(t *testing.T) {
	st := &Stream{opts: Options{FPS: 30}}
	if _, fresh := st.Frame(); fresh {
		t.Error("a stub stream produced a fresh frame")
	}
	if _, err := st.WaitFrame(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("WaitFrame = %v", err)
	}
	if (st.Stats() != Stats{}) {
		t.Errorf("Stats = %+v", st.Stats())
	}
	if st.Options().FPS != 30 {
		t.Errorf("Options = %+v", st.Options())
	}
	if st.Format() != FormatRGBA {
		t.Errorf("Format = %v", st.Format())
	}
	if w, h := st.Size(); w != 0 || h != 0 {
		t.Errorf("Size = %dx%d", w, h)
	}
	if !errors.Is(st.Err(), ErrUnsupported) {
		t.Errorf("Err = %v", st.Err())
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
	old := goos
	t.Cleanup(func() { goos = old })
	goos = "plan9"
	if !strings.Contains(st.String(), "plan9") {
		t.Errorf("String = %q, it should name the platform it is not supported on", st.String())
	}
}

func TestEnvSocketIsTheSameNameEverywhere(t *testing.T) {
	if EnvSocket != "XR_ANDROID_SOCKET" {
		t.Errorf("EnvSocket = %q; the stub and the transport must agree", EnvSocket)
	}
}
