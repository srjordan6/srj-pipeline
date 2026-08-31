package main

import "testing"

// The repair must fix what is broken and leave correct markup untouched.
func TestCloseOpenAnchors(t *testing.T) {
	cases := []struct{ in, want string }{
		// The real defect, from the DCAM page.
		{`<li><a href="https://edmcouncil.org/" rel="noopener">EDM Council</li>`,
			`<li><a href="https://edmcouncil.org/" rel="noopener">EDM Council</a></li>`},
		{`<p>See <a href="/x/">the guide</p>`, `<p>See <a href="/x/">the guide</a></p>`},
		{`<h3>Where does <a href="/y/">DAMA-DMBOK?</h3>`, `<h3>Where does <a href="/y/">DAMA-DMBOK?</a></h3>`},
		// Already correct: must come back byte for byte.
		{`<li><a href="/a/">Fine</a></li>`, `<li><a href="/a/">Fine</a></li>`},
		{`<p><a href="/a/">One</a> and <a href="/b/">two</a></p>`, `<p><a href="/a/">One</a> and <a href="/b/">two</a></p>`},
		{`<p>No links here at all.</p>`, `<p>No links here at all.</p>`},
		// An anchor containing markup is not the shape this repairs.
		{`<li><a href="/a/"><strong>Bold</strong></a></li>`, `<li><a href="/a/"><strong>Bold</strong></a></li>`},
	}
	for _, c := range cases {
		if got := closeOpenAnchors(c.in); got != c.want {
			t.Errorf("closeOpenAnchors(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}
