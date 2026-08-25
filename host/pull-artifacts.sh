#!/bin/sh
# Copyright (c) the go-xrkit/android authors. All rights reserved.
# SPDX-License-Identifier: BSD-3-Clause
#
# Pulls a probe's PNG artefacts off the device to somewhere DURABLE and outside
# every repository.
#
# A screen capture is a picture of whoever ran the probe, at work, and this
# repository is public. The destination is therefore walked up to the filesystem
# root looking for a .git and the pull is REFUSED if one is found. A .gitignore
# is a safety net, not a barrier: `git add -f`, a fresh clone, or any tool that
# does not consult it will publish the file anyway.
#
# Nor is the destination temporary. The artefact exists so that a person can
# look at it, and a scratch directory would be gone before anyone could.
#
#   host/pull-artifacts.sh presentation-own-virtual-display.png
#   XRKIT_ARTIFACT_DIR=/somewhere/else host/pull-artifacts.sh …
set -eu

# adb honours ANDROID_SERIAL itself; there is nothing to pass along.
pkgid=${PACKAGE:-org.goxrkit.androidhost}

# The default follows os.UserConfigDir(), which is what the rest of the fleet
# uses: ~/Library/Application Support on darwin, $XDG_CONFIG_HOME elsewhere.
if [ -n "${XRKIT_ARTIFACT_DIR:-}" ]; then
    dir=$XRKIT_ARTIFACT_DIR
    chose=XRKIT_ARTIFACT_DIR
else
    chose="the default capture directory"
    case $(uname -s) in
        Darwin) base=$HOME/Library/Application\ Support ;;
        *)      base=${XDG_CONFIG_HOME:-$HOME/.config} ;;
    esac
    dir=$base/go-xrkit-android/captures
fi

case $dir in
    /*) ;;
    *)  echo "$chose ($dir) must be an absolute path" >&2; exit 1 ;;
esac

# The refusal is the point. A directory a person chose is still checked, because
# the mistake this prevents is exactly the one a person makes.
p=$dir
while :; do
    if [ -e "$p/.git" ]; then
        echo "REFUSED: $dir is inside the git work tree at $p;" >&2
        echo "         a screen capture must never be written where it can be committed" >&2
        exit 1
    fi
    parent=$(dirname "$p")
    [ "$parent" = "$p" ] && break
    p=$parent
done

mkdir -p "$dir"
for f in "$@"; do
    adb pull "/storage/emulated/0/Android/data/$pkgid/files/$f" "$dir/$f" >/dev/null
    echo "$dir/$f"
done
