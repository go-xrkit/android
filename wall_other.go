// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import "context"

// EnvWallSocket is declared on the Linux side too; see protocol.go.

// WallAvailable reports whether this process is running under a host that
// serves owned displays. Off Android, never.
func WallAvailable() bool { return false }

// openOwnedDisplay is the platform hook behind [Wall.Open]. Off Android there
// is no host to ask, so it reports [ErrUnsupported] — and it does so AFTER the
// wall's own limit check, which is portable and behaves identically everywhere.
func openOwnedDisplay(context.Context, DisplaySpec) (feed, error) {
	return feed{}, ErrUnsupported
}
