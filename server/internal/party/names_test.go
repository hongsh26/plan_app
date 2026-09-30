package party

import (
	"strings"
	"testing"
)

func TestNormalizeNameLengthIsRuneBased(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"40 ascii", strings.Repeat("a", 40), true},
		{"41 ascii", strings.Repeat("a", 41), false},
		{"40 hangul", strings.Repeat("가", 40), true},
		{"41 hangul", strings.Repeat("가", 41), false},
		{"40 decomposed hangul counted after NFC", strings.Repeat("\u1100\u1161", 40), true},
		{"41 decomposed hangul", strings.Repeat("\u1100\u1161", 41), false},
		{"empty", "   ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := NormalizeName(c.in); ok != c.want {
				t.Fatalf("NormalizeName(%q) ok=%v, want %v", c.in, ok, c.want)
			}
		})
	}
}

func TestNormalizeNameCollapsesSpacesAndRejectsControls(t *testing.T) {
	got, ok := NormalizeName("  a    b  ")
	if !ok || got != "a b" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := NormalizeName("a\tb"); ok {
		t.Fatal("tab accepted")
	}
	if _, ok := NormalizeName("a\x00b"); ok {
		t.Fatal("control character accepted")
	}
	if _, ok := NormalizeName("a‮b"); ok {
		t.Fatal("bidi override accepted")
	}
}
