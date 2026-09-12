package main

// Retired before first use, 2026-09-11.
//
// This was going to fetch every cited source itself and summarise it. Then
// reading twoai_industry_hub.go showed the fetching half already existed:
// twoaiHarvestSources has been pulling every cited source URL into
// twoai_source_harvest daily since August, with a content hash and an
// extract, to feed the sector analysis. Building a second fetcher against
// the same URLs would have doubled the traffic we send other people's
// servers to obtain text we already had on disk.
//
// The surviving half - writing a brief per source and keeping it current -
// is twoai_point_briefs.go, which reads that harvest instead. This file is
// left as a note so the next person who has this idea finds the harvest
// first.
