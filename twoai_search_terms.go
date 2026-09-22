package main

// Names shared between a tool and a glossary term, 2026-09-21. Stephen listed
// the terms people search most; two of them mean two things on this site.
// Perplexity is an answer engine and a model accuracy measure; Copilot is
// Microsoft's assistant and a generic kind of assistant. A tool page whose
// name is also a glossary term points across to the other meaning, and the
// glossary entries point back (see_also in site_content resources/glossary.json).
//
// A hub page listing the most searched terms was proposed and dropped the same
// day: Stephen decided the terms belong in the glossary itself, so the five
// platforms became glossary terms instead.

var toolSeeAlso = map[string][]map[string]string{
	"perplexity-ai": {{"name": "Perplexity, the model accuracy measure", "href": "/ai-glossary/perplexity/",
		"note": "The same word in the glossary means how well a model predicts text, lower being better."}},
	"microsoft-copilot": {{"name": "Copilot, the general idea", "href": "/ai-glossary/copilot/",
		"note": "In the glossary, a copilot is any assistant built into existing software that suggests rather than acts."}},
}
