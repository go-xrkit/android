// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import "context"

// Screens returns the displays an application may put a [Screen] on. Off
// Android there is no host to ask, so it reports [ErrUnsupported].
func Screens(context.Context) ([]Display, error) { return nil, ErrUnsupported }

// openScreen is the platform hook behind [ShowOn]. Off Android there is no host
// to ask, so it reports [ErrUnsupported] — and it does so AFTER the presentable
// check and the option validation, which are portable and behave identically
// everywhere.
//
// It is a VAR so the suite can reach ShowOn's success path on a platform that
// has no host at all. A function there would leave the last line of ShowOn
// untestable off Android, and the gate that keeps this stub honest is the same
// 100% gate as everywhere else.
var openScreen = func(context.Context, Display, ScreenOptions) (screenFeed, error) {
	return screenFeed{}, ErrUnsupported
}
