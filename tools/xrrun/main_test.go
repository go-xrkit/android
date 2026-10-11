// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"slices"
	"strings"
	"testing"
)

// ⛔⛔ THIS COST THREE MEASUREMENT CYCLES, EVERY ONE OF THEM AT THE END. Behind
// the keyguard Android does not credit the application as foreground, however
// resumed its Activity is, so the host's own service start throws and the
// process dies in onResume — after a forty-second build and an install. The
// transcripts the fleet keeps of those runs are the fixtures here.
func TestAPhoneThatCannotRunAProbeSaysSo(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want []string
	}{
		{
			// The run of 2026-10-11 that crashed instead of measuring.
			name: "dozing behind the keyguard",
			in: "    mWakefulness=Dozing\n" +
				"    isKeyguardShowing=true\n" +
				"    mScreenOnFully=false\n",
			want: []string{"its screen is off", "it is locked"},
		},
		{
			// ⛔ THE CASE A WAKEFULNESS-ONLY CHECK WOULD HAVE PASSED: the screen
			// is on, the phone is in a pocket under the lock screen, and the
			// service start is refused all the same.
			name: "awake but locked",
			in:   "    mWakefulness=Awake\n    isKeyguardShowing=true\n",
			want: []string{"it is locked"},
		},
		{
			name: "asleep but with no keyguard",
			in:   "    mAwake=false\n    isKeyguardShowing=false\n",
			want: []string{"its screen is off"},
		},
		{
			name: "usable",
			in: "    mWakefulness=Awake\n    isKeyguardShowing=false\n" +
				"    mScreenOnFully=true\n",
			want: nil,
		},
		{
			// ⚠ AN UNREADABLE SECTION IS NOT A REFUSAL. dumpsys gets renamed
			// between releases, and a tool that refused to run because it could
			// not find a field would be worse than the crash it avoids — the
			// crash at least names its own cause.
			name: "a section this tool does not recognise",
			in:   "Window Manager Policy state:\n  something else entirely\n",
			want: nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := lockedBecause(c.in)
			if !slices.Equal(got, c.want) {
				t.Fatalf("lockedBecause = %q, want %q", got, c.want)
			}
		})
	}
}

// Each reason must appear once, or a phone that is both dark and locked is
// described twice and the message reads as two separate problems.
func TestEachReasonIsGivenOnce(t *testing.T) {
	in := "mWakefulness=Dozing\nmScreenOnFully=false\nmAwake=false\n" +
		"isKeyguardShowing=true\nmShowingLockscreen=true\n"
	got := lockedBecause(in)
	if len(got) != 2 {
		t.Fatalf("lockedBecause = %q, want exactly the two reasons", got)
	}
}

// The transcript table is how -app alone is enough, and a name missing from it
// reads as "the probe wrote nothing" — which is also what a crash looks like.
func TestEveryProbeInThisRepositoryHasATranscript(t *testing.T) {
	for _, probe := range []string{"xrusb", "xrcam", "xrscreen", "xrglasses"} {
		if _, ok := transcripts[probe]; !ok {
			t.Errorf("%s writes no transcript this tool knows", probe)
		}
	}
	for probe, file := range transcripts {
		if !strings.HasSuffix(file, ".txt") {
			t.Errorf("%s writes %q, which is not a transcript this tool reads", probe, file)
		}
	}
}

func TestFirstLinesStopsWhereItIsAsked(t *testing.T) {
	// A crash dump is hundreds of lines and the first few name the cause; the
	// rest is the platform's own stack.
	in := []byte("one\ntwo\nthree\nfour\nfive")
	if got := firstLines(in, 3); got != "one\ntwo\nthree" {
		t.Errorf("firstLines = %q", got)
	}
	if got := firstLines(in, 99); got != string(in) {
		t.Errorf("firstLines past the end = %q, want all of it", got)
	}
}
