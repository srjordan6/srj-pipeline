package main

import (
	"os"
	"testing"
)

// Against the real Dallas page Stephen sent, trimmed to the blocks that carry
// a figure. Real bytes rather than a fixture written to pass: a parser test
// built from imagination proves the parser agrees with the imagination.
func TestMetroDallas(t *testing.T) {
	b, err := os.ReadFile("testdata/dallas_metro.json")
	if err != nil {
		t.Skip("fixture not present")
	}
	blocks := tfParseMetro(string(b))
	if len(blocks) < 2 {
		t.Fatalf("expected at least two usable blocks, got %d", len(blocks))
	}
	// DFW16 sits at 1232 Alma Road, Richardson, and the metro page gives it
	// 6.75 MW of IT load and 60,687 square feet of raised floor.
	hit := tfMetroMatch(blocks, 32.966302, -96.715742, "1232", "Alma Road")
	if hit == nil {
		t.Fatal("DFW16 did not match")
	}
	if hit.mw != 6.75 {
		t.Errorf("DFW16 mw = %v, want 6.75", hit.mw)
	}
	if hit.sqft != 60687 {
		t.Errorf("DFW16 sqft = %v, want 60687", hit.sqft)
	}
	// A building nowhere near the page's coordinates must not match at all.
	if tfMetroMatch(blocks, 40.7128, -74.0060, "", "") != nil {
		t.Error("a New York coordinate matched a Dallas block")
	}
}

func TestMetroURL(t *testing.T) {
	got := tfMetroURL("https://www.digitalrealty.com/data-centers/americas/dallas/dfw16")
	want := "https://www.digitalrealty.com/data-centers/americas/dallas"
	if got != want {
		t.Errorf("metro url = %q, want %q", got, want)
	}
	// Must refuse to climb to a page that lists metros rather than buildings.
	if u := tfMetroURL("https://www.digitalrealty.com/data-centers/americas"); u != "" {
		t.Errorf("climbed too far: %q", u)
	}
}

// The Richardson campus is why address beats proximity, and this asserts the
// refusal survives it: a facility with NO address, sitting among three
// neighbours inside 300 metres, must still get no answer rather than one of
// its neighbours' numbers.
func TestMetroRefusesCrowdedCampus(t *testing.T) {
	b, err := os.ReadFile("testdata/dallas_metro.json")
	if err != nil {
		t.Skip("fixture not present")
	}
	blocks := tfParseMetro(string(b))
	if hit := tfMetroMatch(blocks, 32.9670, -96.7130, "", ""); hit != nil {
		t.Errorf("guessed %v MW for a building it cannot identify", hit.mw)
	}
	// And an address the page does not carry must not fall through to a
	// coordinate guess either.
	if hit := tfMetroMatch(blocks, 32.9670, -96.7130, "1", "Nowhere Road"); hit != nil {
		t.Errorf("unknown address matched anyway: %v", hit.addr)
	}
}
