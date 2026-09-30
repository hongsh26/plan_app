package party

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const maxPartyNameRunes = 40

// NormalizeName applies the storage form from party_membership_design.md §4.1.
//
// The length limit is 40 Unicode code points (runes), not grapheme clusters.
// Composite emoji count as several runes; this was decided to avoid a new
// segmentation dependency.
func NormalizeName(raw string) (string, bool) {
	if !utf8.ValidString(raw) {
		return "", false
	}
	s := norm.NFC.String(raw)
	var b strings.Builder
	inSpace := false
	wrote := false
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
		if unicode.IsSpace(r) {
			if wrote {
				inSpace = true
			}
			continue
		}
		if inSpace {
			b.WriteRune(' ')
			inSpace = false
		}
		b.WriteRune(r)
		wrote = true
	}
	name := strings.TrimSpace(b.String())
	if name == "" {
		return "", false
	}
	if utf8.RuneCountInString(name) > maxPartyNameRunes {
		return "", false
	}
	return name, true
}
