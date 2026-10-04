package main

import "testing"

func TestDropPageURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"<!-- saved from url=(0040)https://www.deere.com/en/technology/ai/ -->\n<html>", "https://www.deere.com/en/technology/ai/"},
		{`<html><head><link rel="canonical" href="https://newmont.com/ai"></head>`, "https://newmont.com/ai"},
		{`<meta property="og:url" content="https://www.epri.com/research/ai">`, "https://www.epri.com/research/ai"},
		{`<html>nothing</html>`, "https://yum.com/#saved-digital-and-ai"},
	}
	for _, c := range cases {
		if got := dropPageURL([]byte(c.raw), "yum.com", "Digital and AI.html"); c.want != got && !(c.raw == "<html>nothing</html>" && got == c.want) {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}
