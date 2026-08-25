// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import "runtime"

// goos is a variable rather than runtime.GOOS inline so the stub's String can
// be asserted against a known value from a test on any platform.
var goos = runtime.GOOS
