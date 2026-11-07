// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// Edits are the four things an application packaged by this tool may change
// about the checked-in manifest. They are substituted into a COPY under the
// output directory, so the manifest in the repository stays the demo's.
//
// ⛔ aapt2 CANNOT DO THIS. `--rename-manifest-package` exists and is documented
// as "Renames the package in AndroidManifest.xml file", but no link option sets
// android:label (developer.android.com/tools/aapt2, the link options table,
// read 2026-11-07). An application packaged here should be able to name itself,
// so the manifest is edited rather than the flag used for half of it.
type Edits struct {
	Package     string   // the application id, e.g. "org.goxrkit.xrdesk"
	Label       string   // the launcher label
	Args        string   // the argument string handed to the packaged binary
	Permissions []string // permission names to declare, beyond the host's own
}

// Markers are the exact strings Render replaces. They are literals rather than
// patterns, and each must appear EXACTLY ONCE.
//
// ⛔⛔ THAT COUNT IS THE WHOLE POINT. `sed -e "s|android:label=\"go-xrkit\"|…|"`
// does nothing at all when the manifest no longer spells the label that way,
// and says so by printing the manifest unchanged — a rename in the manifest
// would have silently stopped every packaged application from being able to
// name itself, with a build that still succeeded.
const (
	markerPackage     = `package="org.goxrkit.androidhost"`
	markerLabel       = `android:label="go-xrkit"`
	markerArgs        = `android:name="org.goxrkit.args" android:value=""`
	markerPermissions = `<!-- GW_PERMISSIONS -->`
)

// Render substitutes e into src, which is the checked-in manifest.
func Render(src []byte, e Edits) ([]byte, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	out := string(src)
	for _, sub := range []struct {
		what, marker, with string
	}{
		{"the package id", markerPackage, `package="` + e.Package + `"`},
		{"the label", markerLabel, `android:label="` + attr(e.Label) + `"`},
		{"the application arguments", markerArgs,
			`android:name="org.goxrkit.args" android:value="` + attr(e.Args) + `"`},
		{"the extra permissions", markerPermissions, permissionXML(e.Permissions)},
	} {
		n := strings.Count(out, sub.marker)
		if n != 1 {
			return nil, fmt.Errorf("cannot substitute %s: the manifest holds %d copies of %s, "+
				"want exactly 1. Either the manifest was edited and this marker must follow it, "+
				"or it is the wrong manifest — a substitution that silently matches nothing is "+
				"how a packaged application ends up wearing the demo's name",
				sub.what, n, sub.marker)
		}
		out = strings.Replace(out, sub.marker, sub.with, 1)
	}
	return []byte(out), nil
}

// attr escapes a value for an XML attribute in double quotes.
//
// ⛔ THE SHELL COULD NOT DO THIS, AND WORSE THAN NOT DOING IT. In a `sed`
// replacement an unescaped & means "the whole match", so LABEL='A&B' did not
// produce A&B — it produced the entire matched text in the middle of the label,
// and a label holding a double quote closed the attribute and opened whatever
// followed as markup.
func attr(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// xml.EscapeText only fails when the writer fails, and
		// strings.Builder never does.
		return ""
	}
	// EscapeText leaves the double quote as &#34;, which is correct inside a
	// double-quoted attribute; nothing more is needed.
	return b.String()
}

// permissionXML renders the <uses-permission> elements, one per line, indented
// to match the manifest they are spliced into.
func permissionXML(names []string) string {
	if len(names) == 0 {
		// The comment goes away rather than being left behind: a marker that
		// survives into the built manifest would be substituted twice if this
		// output were ever fed back in.
		return `<!-- no extra permissions: least privilege is the default -->`
	}
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString("\n    ")
		}
		fmt.Fprintf(&b, `<uses-permission android:name="%s" />`, n)
	}
	return b.String()
}

// check refuses the values that cannot be rendered safely. A permission name or
// a package id is not free text: both are Java-style dotted identifiers, and
// anything else arriving here is a mistake upstream — not something to escape
// and pass on.
func (e Edits) check() error {
	if !dottedIdent(e.Package) {
		return fmt.Errorf("package id %q is not a dotted identifier; Android requires at "+
			"least one dot and only letters, digits and underscores in each part", e.Package)
	}
	for _, p := range e.Permissions {
		if !dottedIdent(p) {
			return fmt.Errorf("permission %q is not a dotted identifier: a permission name "+
				"arrives from a command line and is written straight into the manifest, so "+
				"one holding a quote or a space would be declaring something else entirely", p)
		}
	}
	return nil
}

// dottedIdent reports whether s is a non-empty dotted identifier with no empty
// component and at least one dot.
func dottedIdent(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			c := p[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			case c >= '0' && c <= '9' && i > 0:
			default:
				return false
			}
		}
	}
	return true
}
