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
//
// ⛔⛔ THE FIXTURE BELOW IS WHAT THE PHONE PRINTED, not what I expected it to.
// The first version of this test was written from the crash transcript and
// asserted `mWakefulness=Dozing` against `dumpsys window policy` — a string
// that section never prints. Ten expectations, all green, and the guard would
// have passed a phone that was asleep at that moment. Pinning one real
// transcript is the control; see the fleet note about hand-written
// expectations.
const asleepAndLocked = `  mWakefulness=Dozing
  mWakefulnessChanging=false
  mHalInteractiveModeEnabled=false
    mIsInteractive: false
    mKeyguardOccluded=false mPendingKeyguardOccluded=false
    KeyguardServiceDelegate
      showing=true
      inputRestricted=true
      occluded=false
      screenState=SCREEN_STATE_ON
      interactiveState=INTERACTIVE_STATE_SLEEP
`

func TestAPhoneThatCannotRunAProbeSaysSo(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want []string
	}{
		{
			// Measured on the Pixel 11 Pro Fold, 2026-10-11 18:25 CEST.
			name: "asleep and locked, as the phone itself reports it",
			in:   asleepAndLocked,
			want: []string{"it is asleep", "it is locked"},
		},
		{
			// ⛔ THE CASE A WAKEFULNESS-ONLY CHECK WOULD HAVE PASSED: the phone
			// is awake, in a pocket under the lock screen, and the service
			// start is refused all the same.
			name: "awake but locked",
			in:   "  mWakefulness=Awake\n    mIsInteractive: true\n      inputRestricted=true\n",
			want: []string{"it is locked"},
		},
		{
			name: "asleep with the keyguard not restricting",
			in:   "  mWakefulness=Asleep\n      inputRestricted=false\n",
			want: []string{"it is asleep"},
		},
		{
			name: "usable",
			in: "  mWakefulness=Awake\n    mIsInteractive: true\n" +
				"      showing=false\n      inputRestricted=false\n" +
				"      interactiveState=INTERACTIVE_STATE_AWAKE\n",
			want: nil,
		},
		{
			// ⛔ AND THE TRAP THE MEASUREMENT EXPOSED: the screen is ON while
			// this phone sleeps, because of the always-on display. A check that
			// read the screen would have called the fixture above usable.
			name: "the screen is on and the phone is still asleep",
			in:   "  mWakefulness=Dozing\n      screenState=SCREEN_STATE_ON\n",
			want: []string{"it is asleep"},
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

// Each reason must appear once, or a phone that is both asleep and locked is
// described twice and the message reads as two separate problems. The real
// transcript trips several signs for each reason, which is why this matters.
func TestEachReasonIsGivenOnce(t *testing.T) {
	got := lockedBecause(asleepAndLocked + "isKeyguardShowing=true\nmShowingLockscreen=true\n")
	if len(got) != 2 {
		t.Fatalf("lockedBecause = %q, want exactly the two reasons", got)
	}
}

// The transcript table is how -app alone is enough, and a name missing from it
// reads as "the probe wrote nothing" — which is also what a crash looks like.
func TestEveryProbeInThisRepositoryHasATranscript(t *testing.T) {
	for _, probe := range []string{"xrusb", "xrhead", "xrcam", "xrscreen", "xrglasses"} {
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
