package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// AI COMPUTE SUITABILITY, FROM PUBLISHED FACTS.
//
// Stephen, 2026-09-04, after reading a directory that prints an undefined
// "AI score" on every listing: build ours, call it exactly this, and put it
// in the fact box. The difference between theirs and this is the whole
// point. Every input here is a published figure already on the facility
// page with its source; the formula is on its own page; and a facility with
// fewer than three inputs is NOT scored - the box says so - rather than
// defaulting to a number that goes down when data is missing.
//
// Components, each 0 to 1, with weights:
//   capacity   0.35  published IT capacity, MW, log-scaled so that 1 MW is
//                    near 0, 20 MW is about half, 300 MW and above is 1.
//   density    0.30  published cabinet density, kW or kVA per rack. This is
//                    the figure that decides whether accelerators can be
//                    racked at all: under 5 is office-era, 10 to 20 is
//                    air-cooled GPU, 30 and above needs liquid.
//   planned    0.15  announced expansion, MW, same log scale as capacity.
//   operator   0.20  what kind of operator runs the building, from the
//                    registry's own classification: a hyperscaler builds
//                    for its own accelerators, a colocation REIT builds for
//                    tenants who do, an enterprise or academic hall was
//                    built for something else.
//
// The score is the weighted mean of the components present, scaled to 100.
// Three components minimum. What was used is written beside the number.

type twoaiSuitability struct {
	Score      int      `json:"score"`
	Components []string `json:"components"`
	Missing    []string `json:"missing"`
	Basis      string   `json:"basis"`
}

func twoaiLogScale(mw float64, top float64) float64 {
	if mw <= 0 {
		return 0
	}
	v := math.Log10(1+mw) / math.Log10(1+top)
	return math.Max(0, math.Min(1, v))
}

func twoaiDensityScale(kw float64) float64 {
	switch {
	case kw <= 0:
		return 0
	case kw < 5:
		return 0.15
	case kw < 10:
		return 0.4
	case kw < 20:
		return 0.7
	case kw < 30:
		return 0.85
	default:
		return 1
	}
}

var twoaiOperatorTypeScale = map[string]float64{
	"hyperscaler":        1.0,
	"colocation_reit":    0.85,
	"colocation":         0.75,
	"network_carrier":    0.5,
	"enterprise_private": 0.4,
	"government":         0.3,
	"academic":           0.3,
	"facility_services":  0.4,
	"other":              0.4,
}

// twoaiComputeSuitability returns nil when fewer than three inputs exist.
func twoaiComputeSuitability(profile json.RawMessage, mw float64, operatorType string) *twoaiSuitability {
	var p map[string]any
	_ = json.Unmarshal(profile, &p)
	num := func(k string) float64 {
		switch v := p[k].(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(v, ",", "")), 64); err == nil {
				return f
			}
		}
		return 0
	}
	cap := mw
	if cap <= 0 {
		cap = num("it_capacity_mw")
	}
	density := num("power_density_kw_rack")
	if density == 0 {
		density = num("kw_per_rack")
	}
	planned := num("planned_it_capacity_mw")
	opScale, opKnown := twoaiOperatorTypeScale[strings.ToLower(operatorType)]

	type comp struct {
		name   string
		weight float64
		value  float64
		ok     bool
	}
	comps := []comp{
		{"published IT capacity", 0.35, twoaiLogScale(cap, 300), cap > 0},
		{"published rack density", 0.30, twoaiDensityScale(density), density > 0},
		{"announced expansion", 0.15, twoaiLogScale(planned, 300), planned > 0},
		{"operator type", 0.20, opScale, opKnown && operatorType != ""},
	}
	var used, missing []string
	var num1, den float64
	for _, c := range comps {
		if c.ok {
			used = append(used, c.name)
			num1 += c.weight * c.value
			den += c.weight
		} else {
			missing = append(missing, c.name)
		}
	}
	if len(used) < 3 || den == 0 {
		return nil
	}
	score := int(math.Round(100 * num1 / den))
	return &twoaiSuitability{
		Score: score, Components: used, Missing: missing,
		Basis: "weighted mean of the published inputs named here, scaled to 100; formula on the methodology page",
	}
}
