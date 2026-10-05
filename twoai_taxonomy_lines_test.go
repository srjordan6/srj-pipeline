package main

import (
	"strings"
	"testing"
)

func TestTwoaiOneLine(t *testing.T) {
	short := "Who makes the memory on an AI accelerator."
	if got := twoaiOneLine(short); got != short {
		t.Errorf("short text changed: %q", got)
	}
	long := "Power and cooling set how much compute a data center can run. The U.S. grid adds capacity slowly, and liquid cooling is now standard for the densest racks. " + strings.Repeat("More detail follows here. ", 20)
	got := twoaiOneLine(long)
	if got != "Power and cooling set how much compute a data center can run." {
		t.Errorf("first sentence: %q", got)
	}
	abbrev := "Rules from the U.S. Congress and the E.U. Parliament shape how AI is built across many markets today, and the details matter a great deal for anyone shipping models. " + strings.Repeat("x ", 200)
	if got := twoaiOneLine(abbrev); strings.HasPrefix(got, "Rules from the U.S.") && len(got) < 30 {
		t.Errorf("cut at an abbreviation: %q", got)
	}
	noStop := strings.Repeat("word ", 100)
	if got := twoaiOneLine(noStop); len(got) > twoaiLineMax+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("no sentence end: %q", got)
	}
}
