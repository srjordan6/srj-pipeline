package main

// twoai_health_watch: new primary-source items on diabetes and on
// lipoprotein(a), every run, for the content project's medical hubs.
//
// Requested by theworldofai for Stephen on 2026-10-05, first for diabetes and
// the same day widened to Lp(a) (bridge row 484). Stephen wants both kept
// current daily, so the stage runs on every pipeline run and sends one
// bridge row per topic per calendar day, and only when there is something
// new to read.
//
// WHAT COUNTS AS NEW. Every item is keyed by its URL in twoai_health_watch
// and inserted with ON CONFLICT DO NOTHING, so a row is new exactly once,
// the first time any run sees it. The bridge row counts rows of that topic
// still at status 'new' that arrived after the previous bridge row.
//
// PRIMARY SOURCES FIRST, COVERAGE MARKED. PubMed, ClinicalTrials.gov, FDA,
// EMA, the journals' own feeds and the companies' and societies' own
// newsrooms are primary. Where a company or society has no feed that works,
// a Google News query stands in, and those rows carry kind 'coverage' and a
// source ending "(coverage)", so nobody mistakes a news report for the
// company's own words. Every feed below was fetched with curl on 2026-10-05
// before it was listed, and the ones that failed are recorded beside the
// lists, so a missing newsroom is a written fact and not an oversight.
//
// A JOIN MUST STORE WHAT IT MATCHED ON (AGENTS.md rule 4). The topic gate,
// the sub-hub classifier and the verify tagger each write the words that
// fired into detail, so a wrong classification can be seen and fixed.
//
// NOTHING HERE CALLS A MODEL. Classification is keyword tables, tested in
// twoai_health_watch_test.go, so the stage is neither a bulk stage nor
// daily-only.

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	twoaiHealthStage = "twoai_health_watch"
	twoaiHealthUA    = "srj-pipeline/1.0 (srjconsultingservices.com)"
	// NCBI asks every E-utilities caller to identify itself with tool and
	// email, and to stay at or under three requests a second without a key.
	twoaiHealthNCBITool  = "srj-pipeline"
	twoaiHealthNCBIEmail = "srj@srjconsultingservices.com"
	// Google News redirect links resolved to the publisher per run. Each
	// costs two requests, so this caps the scraping, not the rows: an
	// unresolved row keeps its Google link and is still deduplicated.
	twoaiHealthResolveBudget = 40
	// Time kept back from the stage deadline for the bridge rows and the
	// summary line, so fetching stops cleanly before the runner kills us.
	twoaiHealthReserve = 90 * time.Second
)

// ---------------------------------------------------------------------
// Topic gates: does an item from a general source belong here at all?
// ---------------------------------------------------------------------

// twoaiHealthTopicTerms decides the topic of an item from a general source
// (FDA, EMA, a general journal, a company newsroom). Lp(a) is tested first
// because it is the narrower term set: a cardiology item that mentions both
// is about Lp(a) for this purpose.
var twoaiHealthTopicTerms = []struct {
	topic string
	re    *regexp.Regexp
}{
	{"lpa", regexp.MustCompile(`(?i)(lipoprotein\s*\(a\)|\blp\s*\(a\)|\blipoprotein a\b|apolipoprotein\s*\(a\)|\bapo\s*\(a\)|pelacarsen|olpasiran|lepodisiran|zerlasiran|muvalaplin|\bctx-?320\b|\bsln-?360\b|\btqj-?230\b)`)},
	{"diabetes", regexp.MustCompile(`(?i)(diabet\w*|\bglyc(a)?emi\w*|\bhba1c\b|\ba1c\b|\bglucose\b|\binsulin\w*|\bobes\w*|\boverweight\b|\bweight[- ](loss|management|reduction)|anti-?obesity|\bglp-?1\b|glucagon-like peptide|\bgip\b|\bincretin\w*|semaglutide|tirzepatide|liraglutide|dulaglutide|exenatide|retatrutide|orforglipron|amycretin|cagrilintide|cagrisema|maridebart|maritide|survodutide|mazdutide|\bamylin\b|pramlintide|\bsglt-?2\b|\w+gliflozin\b|teplizumab|\bislets?\b|\bbeta[- ]cells?\b|zimislecel|\bcgm\b|ozempic|wegovy|mounjaro|zepbound|rybelsus|foundayo|saxenda|trulicity|afrezza|omnipod|freestyle libre|\bdexcom\b|minimed|\bketones?\b|ketoacidosis|metformin|\bpcos\b|polycystic ovary|\bmetabolic\b)`)},
}

// twoaiHealthTopicOf returns the first topic whose terms appear in text, with
// the words that matched, limited to the topics the source covers. An empty
// allowed list means any topic.
func twoaiHealthTopicOf(text string, allowed []string) (string, string) {
	for _, t := range twoaiHealthTopicTerms {
		if len(allowed) > 0 && !hwHas(allowed, t.topic) {
			continue
		}
		if m := t.re.FindString(text); m != "" {
			return t.topic, m
		}
	}
	return "", ""
}

func hwHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Sub-hub classifiers. First matching rule wins, so the order is the
// precedence and it is deliberate.
// ---------------------------------------------------------------------

type hwRule struct {
	hub string
	re  *regexp.Regexp
}

// twoaiHealthDiabetesHubs places a diabetes item on one of the content
// project's ten sub-hubs. Precedence, highest first, and why:
//
//	dmed-tech           devices are named unambiguously, and a CGM study in
//	                    kidney patients is a device story first.
//	dmed-insulin-beta   islet and beta-cell replacement and the new weekly
//	                    insulins, before type 1 so a T1D islet paper lands on
//	                    the cell therapy hub.
//	dmed-type1          teplizumab, autoimmunity and type 1 populations.
//	dmed-kidney,        outcome hubs before drug hubs, so "semaglutide and
//	dmed-heart          kidney outcomes" is a kidney story.
//	dmed-nextgen        named next-generation agents before the GLP-1 class,
//	                    so "CagriSema versus semaglutide" is next-gen.
//	dmed-glp1           the marketed incretins and the class words.
//	dmed-weight         obesity, remission, bariatrics.
//	dmed-glucose        glycaemic control; also claims "insulin resistance"
//	                    before the generic insulin rule below can.
//	dmed-insulin-beta   any other insulin item.
//	dmed-pipeline       early phase work with nothing more specific.
//
// Anything else falls back to dmed.
var twoaiHealthDiabetesHubs = []hwRule{
	{"dmed-tech", regexp.MustCompile(`(?i)\bcgms?\b|continuous glucose|glucose monitor\w*|glucose sensor\w*|insulin pumps?|patch pump|pump therapy|automated insulin|closed[- ]loop|artificial pancreas|ketone monitor\w*|glucose[- ]ketone|continuous ketone|freestyle libre|\blibre\b|\bdexcom\b|omnipod|minimed|smart (insulin )?pens?|connected pens?`)},
	{"dmed-insulin-beta", regexp.MustCompile(`(?i)\bislets?\b|\bbeta[- ]cells?\b|β[- ]cells?|stem[- ]cell[- ]derived|zimislecel|vx-880|icodec|efsitora|weekly insulin|basal insulin|inhaled insulin|afrezza|technosphere`)},
	{"dmed-type1", regexp.MustCompile(`(?i)teplizumab|tzield|type 1 diabet\w*|\bt1d\b|\bt1dm\b|autoimmun\w*|\blada\b`)},
	{"dmed-kidney", regexp.MustCompile(`(?i)kidney|\brenal\b|\bckd\b|\bdkd\b|nephropath\w*|albuminuri\w*|\begfr\b|finerenone|dialysis`)},
	{"dmed-heart", regexp.MustCompile(`(?i)cardiovascular|\bcardiac\b|\bmace\b|heart failure|\bhfpef\b|\bhfref\b|myocardial|\bstroke\b|coronary|atheroscl\w*|surpass-cvot|\bcvot\b`)},
	{"dmed-nextgen", regexp.MustCompile(`(?i)retatrutide|orforglipron|foundayo|amycretin|cagrilintide|cagrisema|maritide|maridebart|survodutide|mazdutide|ecnoglutide|pemvidutide|petrelintide|triple agonist|\bamylin\b`)},
	{"dmed-glp1", regexp.MustCompile(`(?i)\bglp-?1\b|glucagon-like peptide|semaglutide|tirzepatide|liraglutide|dulaglutide|exenatide|lixisenatide|\bincretin\w*|\bgip\b|ozempic|wegovy|mounjaro|zepbound|rybelsus|trulicity|saxenda|victoza`)},
	{"dmed-weight", regexp.MustCompile(`(?i)\bobes\w*|overweight|\bweight\b|\bbmi\b|remission|bariatric|adipos\w*`)},
	{"dmed-glucose", regexp.MustCompile(`(?i)\bhba1c\b|\ba1c\b|glyc(a)?emi\w*|hypoglyc\w*|hyperglyc\w*|\bglucose\b|insulin resistance|insulin sensitivity|metformin|\bsglt-?2\b|\w+gliflozin\b`)},
	{"dmed-insulin-beta", regexp.MustCompile(`(?i)\binsulins?\b`)},
	{"dmed-pipeline", regexp.MustCompile(`(?i)\bphase ?(1|2|i|ii)\b|phase 1/2|first-in-human|\bpipeline\b|preclinical|early[- ]stage`)},
}

// twoaiHealthLpaHubs places an Lp(a) item. Precedence, highest first:
// aortic valve disease is specific and easily lost under "cardiovascular",
// named outcome trials come before the drug that is being tested in them,
// then the three therapy routes (gene editing, oral, RNA), then testing and
// guidelines, cardiovascular risk, current therapies, the biology, and
// finally early work. Anything else falls back to lpa.
var twoaiHealthLpaHubs = []hwRule{
	{"lpa-aortic", regexp.MustCompile(`(?i)aortic stenosis|aortic valve|valve calcification|valvular|calcific aortic|\bcavs\b|\btavr\b|\btavi\b`)},
	{"lpa-trials", regexp.MustCompile(`(?i)outcomes? trial|horizon|ocean\s*\(a\)|\bocean\b|acclaim|move-lp|\bnct\d{8}\b|\bphase ?(3|iii)\b|cardiovascular outcomes`)},
	{"lpa-gene", regexp.MustCompile(`(?i)\bctx-?320\b|crispr|gene[- ]edit\w*|base[- ]edit\w*|gene therap\w*|\bverve\b|one[- ]time treatment`)},
	{"lpa-oral", regexp.MustCompile(`(?i)muvalaplin|\boral\b|small[- ]molecule|\bpills?\b|\btablets?\b`)},
	{"lpa-rna", regexp.MustCompile(`(?i)pelacarsen|olpasiran|lepodisiran|zerlasiran|\bsirna\b|antisense|small interfering|rna interference|\brnai\b|\baso\b|\bsln-?360\b|\btqj-?230\b`)},
	{"lpa-testing", regexp.MustCompile(`(?i)guideline\w*|testing|\btest(s|ed)?\b|measur\w*|assay\w*|screening|\bacc\b|\baha\b|\besc\b|\beas\b|recommendation\w*|consensus|nmol/l|mg/dl|standardi[sz]\w*|cascade`)},
	{"lpa-cvd", regexp.MustCompile(`(?i)cardiovascular|coronary|myocardial infarction|heart attack|\bstroke\b|atheroscl\w*|\bascvd\b|\bmace\b|peripheral arter\w*|heart disease|\bcardiac\b`)},
	{"lpa-current", regexp.MustCompile(`(?i)statins?\b|\bpcsk9\b|evolocumab|alirocumab|inclisiran|apheresis|niacin|bempedoic|ezetimibe|lipid[- ]lowering|current (therap|treat)\w*`)},
	{"lpa-understanding", regexp.MustCompile(`(?i)genetic\w*|\blpa gene\b|\blpa\b|kringle|apolipoprotein\s*\(a\)|ancestry|inherit\w*|heritab\w*|mechanis\w*|pathophysiolog\w*|oxidi[sz]ed phospholipid\w*|isoform\w*`)},
	{"lpa-future", regexp.MustCompile(`(?i)\bnovel\b|emerging|\bfuture\b|investigational|\bpipeline\b|\bphase ?(1|2|i|ii)\b|first-in-human|early[- ]phase`)},
}

// twoaiHealthSubHub returns the sub-hub for an item and the words that put it
// there. An item with no rule match gets the topic's fallback hub and an
// empty match.
func twoaiHealthSubHub(topic, text string) (string, string) {
	rules, fallback := twoaiHealthDiabetesHubs, "dmed"
	if topic == "lpa" {
		rules, fallback = twoaiHealthLpaHubs, "lpa"
	}
	for _, r := range rules {
		if m := r.re.FindString(text); m != "" {
			return r.hub, m
		}
	}
	return fallback, ""
}

// ---------------------------------------------------------------------
// The verify list: unpublished claims the content project wants confirmed
// at a primary source before it prints them. Each key has a PubMed query,
// a Google News query, or both, and a pattern that tags an item from ANY
// source, so a company press release confirming a claim is found as well
// as a news report about it. Rows carry detail->>'verify' = key.
// ---------------------------------------------------------------------

type hwVerify struct {
	key, topic, pubmed, gnews string
	re                        *regexp.Regexp
}

var twoaiHealthVerify = []hwVerify{
	{"abbott-ketone-monitor", "diabetes",
		`(ketone[tiab] AND continuous[tiab] AND (monitor*[tiab] OR sensor*[tiab]))`,
		`Abbott ("ketone monitor" OR "glucose-ketone" OR "Libre Duo" OR "continuous ketone")`,
		regexp.MustCompile(`(?i)(abbott|libre).*ketone|ketone.*(abbott|libre)|glucose[- ]ketone|continuous ketone|(monitor\w*|wearable|sensor\w*|authori[sz]\w*|clear\w*).*\bketones?\b|\bketones?\b.*(monitor\w*|wearable|sensor\w*)`)},
	{"afrezza-pediatric", "diabetes",
		`(("inhaled insulin"[tiab] OR "technosphere insulin"[tiab] OR afrezza[tiab]) AND (pediatric[tiab] OR paediatric[tiab] OR children[tiab] OR adolescents[tiab]))`,
		`Afrezza (pediatric OR children OR "INHALE-1")`,
		regexp.MustCompile(`(?i)(afrezza|inhaled insulin|technosphere).*(pediatric|paediatric|child|adolescent|inhale-1)|inhale-1`)},
	{"amgen-maritide", "diabetes",
		`(maridebart[tiab] OR MariTide[tiab] OR "AMG 133"[tiab])`,
		`MariTide OR "maridebart cafraglutide"`,
		regexp.MustCompile(`(?i)maritide|maridebart|maritime-\d`)},
	{"pcos-glp1-metformin", "diabetes",
		`(("polycystic ovary syndrome"[tiab] OR PCOS[tiab]) AND (GLP-1[tiab] OR "glucagon-like peptide-1"[tiab] OR semaglutide[tiab] OR liraglutide[tiab] OR exenatide[tiab]) AND metformin[tiab])`,
		`"polyendocrine metabolic ovarian syndrome" OR PMOS OR (PCOS GLP-1 metformin)`,
		regexp.MustCompile(`(?i)(pcos|polycystic ovar\w*).*(glp-?1|semaglutide|liraglutide|exenatide)|(glp-?1|semaglutide|liraglutide|exenatide).*(pcos|polycystic ovar\w*)|\bpmos\b|polyendocrine metabolic ovarian`)},
	{"us-glp1-usage", "diabetes",
		`((GLP-1[tiab] OR "glucagon-like peptide-1"[tiab] OR semaglutide[tiab] OR tirzepatide[tiab]) AND (prevalence[tiab] OR utilization[tiab] OR uptake[tiab]) AND ("United States"[tiab] OR NHANES[tiab] OR nationally[tiab]))`,
		`(GLP-1 OR Ozempic OR Wegovy OR Zepbound) (Americans OR "U.S. adults") (poll OR survey OR percent)`,
		regexp.MustCompile(`(?i)(glp-?1|ozempic|wegovy|zepbound|weight[- ]loss (drug|shot)s?).*(americans|u\.s\. adults|us adults|nationally representative|nhanes|\bkff\b|gallup)|(americans|u\.s\. adults|us adults|\bkff\b|gallup).*(glp-?1|ozempic|wegovy|zepbound|weight[- ]loss (drug|shot)s?)`)},
	{"vertex-zimislecel", "diabetes",
		`(zimislecel[tiab] OR "VX-880"[tiab])`,
		`zimislecel OR "VX-880"`,
		regexp.MustCompile(`(?i)zimislecel|vx-880`)},
	{"t1d-gene-therapy", "diabetes",
		`("type 1 diabetes"[tiab] AND ("gene therapy"[tiab] OR "gene editing"[tiab] OR "gene-edited"[tiab] OR hypoimmune[tiab]))`,
		`"type 1 diabetes" ("gene therapy" OR "gene-edited" OR "gene editing" OR hypoimmune) trial`,
		regexp.MustCompile(`(?i)(gene therap\w*|gene[- ]edit\w*|hypoimmune).*(type 1|t1d|islet)|(type 1|t1d|islet).*(gene therap\w*|gene[- ]edit\w*|hypoimmune)`)},
	{"mounjaro-cv-indication", "diabetes",
		`("SURPASS-CVOT"[tiab] OR (tirzepatide[tiab] AND "cardiovascular outcomes"[tiab]))`,
		`(Mounjaro OR tirzepatide) cardiovascular (FDA OR indication OR "SURPASS-CVOT")`,
		regexp.MustCompile(`(?i)surpass-cvot|(tirzepatide|mounjaro|zepbound).*(cardiovascular|\bmace\b|\bheart\b)`)},
	{"cagrisema-reimagine2", "diabetes",
		`(CagriSema[tiab] OR (cagrilintide[tiab] AND semaglutide[tiab]))`,
		`CagriSema ("REIMAGINE 2" OR FDA OR submission)`,
		regexp.MustCompile(`(?i)reimagine|cagrisema.*(fda|submi\w*|filing|filed|regulator\w*|approv\w*)`)},
	{"retatrutide-filing", "diabetes",
		"",
		`retatrutide (FDA OR submission OR filing OR regulatory)`,
		regexp.MustCompile(`(?i)retatrutide.*(fda|submi\w*|filing|filed|regulator\w*|approv\w*)`)},
	{"lpa-horizon-presentation", "lpa",
		`(pelacarsen[tiab] OR "Lp(a)HORIZON"[tiab] OR TQJ230[tiab])`,
		`("Lp(a)HORIZON" OR pelacarsen) (results OR outcome OR presented OR presentation)`,
		regexp.MustCompile(`(?i)lp\s*\(a\)\s*horizon|horizon.*pelacarsen|pelacarsen.*(outcome|result\w*|horizon|\bmace\b|present\w*|topline|fail\w*|setback|phase 3|\bdata\b)|(fail\w*|setback).*pelacarsen`)},
}

// twoaiHealthVerifyKey tags an item with the first verify key of its topic
// whose pattern matches, and returns the words that matched.
func twoaiHealthVerifyKey(topic, text string) (string, string) {
	for _, v := range twoaiHealthVerify {
		if v.topic != topic {
			continue
		}
		if m := v.re.FindString(text); m != "" {
			return v.key, m
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------
// Sources. Each list was verified with curl on 2026-10-05.
// ---------------------------------------------------------------------

// PubMed watch queries, one esearch each over the last three days of entry
// dates. The human filter drops records indexed as animal-only and keeps
// everything not yet MeSH-indexed, which is every brand new record, so it
// removes mouse studies over time without hiding anything new. Probed
// 2026-10-05 over three days: incretins 39, SGLT2 27, insulins 1, type 1 and
// cell therapy 1, devices 5, kidney 9, and Lp(a) 23 over seven days.
var twoaiHealthPubmed = []struct{ topic, key, term string }{
	{"diabetes", "incretins", `(GLP-1[tiab] OR "glucagon-like peptide-1"[tiab] OR "glucose-dependent insulinotropic"[tiab] OR GIP[tiab] OR tirzepatide[tiab] OR semaglutide[tiab] OR liraglutide[tiab] OR dulaglutide[tiab] OR retatrutide[tiab] OR orforglipron[tiab] OR amylin[tiab] OR cagrilintide[tiab] OR CagriSema[tiab] OR amycretin[tiab] OR survodutide[tiab] OR maridebart[tiab] OR mazdutide[tiab])`},
	{"diabetes", "sglt2", `(SGLT2[tiab] OR "SGLT-2"[tiab] OR "sodium-glucose cotransporter 2"[tiab] OR empagliflozin[tiab] OR dapagliflozin[tiab] OR canagliflozin[tiab] OR sotagliflozin[tiab])`},
	{"diabetes", "insulins", `("insulin icodec"[tiab] OR icodec[tiab] OR efsitora[tiab] OR "once-weekly insulin"[tiab] OR "weekly insulin"[tiab] OR "inhaled insulin"[tiab])`},
	{"diabetes", "type1-cells", `(teplizumab[tiab] OR "islet transplantation"[tiab] OR "islet transplant"[tiab] OR "beta cell replacement"[tiab] OR "beta-cell replacement"[tiab] OR "stem cell-derived islets"[tiab] OR zimislecel[tiab])`},
	{"diabetes", "devices", `("continuous glucose monitoring"[tiab] OR "continuous glucose monitor"[tiab] OR "automated insulin delivery"[tiab] OR "insulin pump"[tiab] OR "closed-loop insulin"[tiab])`},
	{"diabetes", "kidney", `("diabetic kidney disease"[tiab] OR "diabetic nephropathy"[tiab])`},
	{"lpa", "lpa", `("lipoprotein(a)"[tiab] OR "Lp(a)"[tiab] OR pelacarsen[tiab] OR olpasiran[tiab] OR lepodisiran[tiab] OR zerlasiran[tiab] OR muvalaplin[tiab] OR CTX320[tiab])`},
}

const twoaiHealthHumanFilter = ` NOT (animals[mh] NOT humans[mh])`

// twoaiHealthTrackedTrials are the Lp(a) outcome trials the content project
// follows by name. Each update to one of them is a new item, so their row
// URL carries the update date as a fragment (the link still opens the
// study). MOVE-Lp(a), muvalaplin, is NCT07157774, found through the
// ClinicalTrials.gov API on 2026-10-05.
var twoaiHealthTrackedTrials = []string{"NCT04023552", "NCT05581303", "NCT06292013", "NCT07157774"}

// twoaiHealthFeed is one RSS or Atom feed. With filter set, an item is kept
// only when a topic term from topics appears in its title or summary.
// Without it, every item is kept and gets the matched topic, or topics[0].
type twoaiHealthFeed struct {
	source, url, kind string
	topics            []string
	filter            bool
}

var hwBoth = []string{"diabetes", "lpa"}

var twoaiHealthFeeds = []twoaiHealthFeed{
	// FDA press announcements, filtered. 20 items on 2026-10-05.
	{"FDA", "https://www.fda.gov/about-fda/contact-fda/stay-informed/rss-feeds/press-releases/rss.xml", "fda", hwBoth, true},
	// EMA. news.xml carries news and press releases (8 items), and the two
	// medicine feeds carry new CHMP opinions (12) and EPAR updates (153),
	// whose titles name the active substance. Listed on
	// ema.europa.eu/en/news-events/rss-feeds; /en/rss.xml is a 404.
	{"EMA", "https://www.ema.europa.eu/en/news.xml", "ema", hwBoth, true},
	{"EMA", "https://www.ema.europa.eu/en/new-human-medicine-new.xml", "ema", hwBoth, true},
	{"EMA", "https://www.ema.europa.eu/en/human-medicine-new.xml", "ema", hwBoth, true},
	// Journals. The diabetes journals are kept whole, the general and the
	// endocrinology ones are filtered. NEJM and the Lancet feeds are RSS 1.0.
	{"Diabetes Care", "https://diabetesjournals.org/rss/site_1000003/1000004.xml", "journal", []string{"diabetes", "lpa"}, false},
	{"Diabetes Care", "https://diabetesjournals.org/rss/site_1000003/advanceAccess_1000004.xml", "journal", []string{"diabetes", "lpa"}, false},
	{"Diabetologia", "https://link.springer.com/search.rss?facet-content-type=Article&facet-journal-id=125", "journal", []string{"diabetes", "lpa"}, false},
	{"The Lancet Diabetes & Endocrinology", "https://www.thelancet.com/rssfeed/landia_online.xml", "journal", hwBoth, true},
	{"The Lancet Diabetes & Endocrinology", "https://www.thelancet.com/rssfeed/landia_current.xml", "journal", hwBoth, true},
	{"NEJM", "https://www.nejm.org/action/showFeed?jc=nejm&type=etoc&feed=rss", "journal", hwBoth, true},
	{"The Lancet", "https://www.thelancet.com/rssfeed/lancet_online.xml", "journal", hwBoth, true},
	{"The Lancet", "https://www.thelancet.com/rssfeed/lancet_current.xml", "journal", hwBoth, true},
	// Atherosclerosis is the EAS journal, read for Lp(a) only.
	{"Atherosclerosis (EAS)", "https://www.atherosclerosis-journal.com/current.rss", "journal", []string{"lpa"}, true},
	{"Atherosclerosis (EAS)", "https://www.atherosclerosis-journal.com/inpress.rss", "journal", []string{"lpa"}, true},
	// First-party company newsrooms, filtered to the topics each company
	// works in.
	{"Eli Lilly", "https://investor.lilly.com/rss/news-releases.xml", "company", hwBoth, true},
	{"Amgen", "https://investors.amgen.com/rss/news-releases.xml", "company", hwBoth, true},
	{"Vertex", "https://investors.vrtx.com/rss/news-releases.xml", "company", []string{"diabetes"}, true},
	{"Abbott", "https://abbott.mediaroom.com/press-releases?pagetemplate=rss", "company", []string{"diabetes"}, true},
	{"Medtronic", "https://news.medtronic.com/press-releases?pagetemplate=rss", "company", []string{"diabetes"}, true},
	// MiniMed is Medtronic's diabetes business under its own newsroom.
	{"MiniMed (Medtronic Diabetes)", "https://news.minimed.com/press-releases?pagetemplate=rss", "company", []string{"diabetes"}, true},
	// Sanofi's global site has no feed; its US newsroom does.
	{"Sanofi US", "https://www.news.sanofi.us/press-releases?pagetemplate=rss", "company", []string{"diabetes"}, true},
	{"MannKind", "https://investors.mannkindcorp.com/rss/news-releases.xml", "company", []string{"diabetes"}, true},
	{"Ionis", "https://ir.ionis.com/rss/news-releases.xml", "company", []string{"lpa"}, true},
	// CRISPR Therapeutics runs CTX320 for Lp(a) and an islet programme.
	{"CRISPR Therapeutics", "https://ir.crisprtx.com/rss/news-releases.xml", "company", hwBoth, true},
	// Societies with a working first-party feed.
	{"American Heart Association", "https://newsroom.heart.org/rss.xml", "society", hwBoth, true},
	{"European Atherosclerosis Society", "https://eas-society.org/feed/", "society", []string{"lpa"}, true},
}

// NEWSROOMS WITH NO WORKING FEED, probed 2026-10-05, covered below through
// Google News and marked as coverage:
//
//	Novo Nordisk   novonordisk.com .rss paths 404, novonordisk-us.com 404,
//	               the GlobeNewswire organisation feed did not answer.
//	Dexcom         investors.dexcom.com rss paths all 404.
//	Insulet        investors.insulet.com rss paths all 404.
//	Zealand Pharma zealandpharma.com answers 403 to every feed path.
//	Sanofi global  sanofi.com press release rss paths 404 (US newsroom above).
//	Novartis       novartis.com rss paths 404.
//	Silence Ther.  ir.silence-therapeutics.com did not answer at any path.
//	ACC            acc.org rss paths 404, the JACC etoc feed 404.
//	ESC            escardio.org rss paths 404, no feed on the press page.
//
// With gate set, a story is kept only when a topic term is in its headline.
// Dexcom, Insulet and Zealand do nothing but diabetes and obesity, and
// their headlines often name only the company ("Insulet raises guidance"),
// so for them the query is the filter: on 2026-10-05 the gate kept 0 of 8
// Insulet stories.
var twoaiHealthCoverage = []struct {
	source, query, window string
	topics                []string
	gate                  bool
}{
	{"Novo Nordisk", `"Novo Nordisk" (diabetes OR obesity OR semaglutide OR Ozempic OR Wegovy OR CagriSema OR amycretin OR insulin)`, "3d", []string{"diabetes"}, true},
	{"Sanofi", `Sanofi (diabetes OR insulin OR Tzield OR teplizumab)`, "3d", []string{"diabetes"}, true},
	{"Dexcom", `Dexcom`, "3d", []string{"diabetes"}, false},
	{"Insulet", `Insulet OR Omnipod`, "3d", []string{"diabetes"}, false},
	{"Zealand Pharma", `"Zealand Pharma" OR petrelintide OR dapiglutide`, "3d", []string{"diabetes"}, false},
	{"Novartis", `Novartis (pelacarsen OR "Lp(a)" OR "lipoprotein(a)")`, "7d", []string{"lpa"}, true},
	{"Silence Therapeutics", `"Silence Therapeutics" OR zerlasiran`, "7d", []string{"lpa"}, true},
	{"ACC", `("American College of Cardiology" OR ACC) ("lipoprotein(a)" OR "Lp(a)")`, "14d", []string{"lpa"}, true},
	{"ESC", `("European Society of Cardiology" OR ESC) ("lipoprotein(a)" OR "Lp(a)")`, "14d", []string{"lpa"}, true},
}

// openFDA ingredient names for the drug approvals query. "insulin" matches
// every insulin, efsitora and icodec included, because openFDA tokenises
// the name.
var twoaiHealthFDADrugs = []string{
	"semaglutide", "tirzepatide", "liraglutide", "dulaglutide", "exenatide", "lixisenatide",
	"retatrutide", "orforglipron", "cagrilintide", "survodutide", "maridebart", "pramlintide",
	"insulin", "empagliflozin", "dapagliflozin", "canagliflozin", "ertugliflozin", "sotagliflozin",
	"bexagliflozin", "teplizumab", "finerenone", "donislecel", "zimislecel",
	"pelacarsen", "olpasiran", "lepodisiran", "zerlasiran", "muvalaplin",
}

// ---------------------------------------------------------------------
// The run.
// ---------------------------------------------------------------------

type hwItem struct {
	url, source, title, date, kind, topic string
	// classify is the text the sub-hub and verify rules read, when it
	// should be more than the title.
	classify string
	detail   map[string]any
}

type hwRun struct {
	db       *sql.DB
	stop     time.Time
	ncbiKey  string
	lastNCBI time.Time
	resolved int
	seen     map[string]int
	added    map[string]int
	byTopic  map[string]int
	notices  []string
	stopped  bool
}

// hwFamily maps a row kind to the counter it reports under.
func hwFamily(kind string) string {
	switch kind {
	case "trial":
		return "trials"
	case "journal":
		return "journals"
	case "company":
		return "companies"
	case "society":
		return "societies"
	}
	return kind
}

func (r *hwRun) late(step string) bool {
	if time.Now().After(r.stop) {
		if !r.stopped {
			r.notices = append(r.notices, "deadline reached before "+step+", stopped fetching")
		}
		r.stopped = true
		return true
	}
	return false
}

func (r *hwRun) notice(format string, a ...any) {
	r.notices = append(r.notices, fmt.Sprintf(format, a...))
}

func (r *hwRun) get(u string) ([]byte, error) {
	return twoaiJobsGet(u, map[string]string{"User-Agent": twoaiHealthUA})
}

// save writes one item and reports whether it was new.
func (r *hwRun) save(it hwItem) bool {
	if it.url == "" || it.title == "" || it.topic == "" {
		return false
	}
	if it.detail == nil {
		it.detail = map[string]any{}
	}
	text := it.title
	if it.classify != "" {
		text = it.classify
	}
	hub, why := twoaiHealthSubHub(it.topic, text)
	if why != "" {
		it.detail["sub_hub_matched"] = why
	}
	if _, ok := it.detail["verify"]; !ok {
		if k, m := twoaiHealthVerifyKey(it.topic, text); k != "" {
			it.detail["verify"] = k
			it.detail["verify_matched"] = m
			it.detail["verify_via"] = "pattern"
		}
	}
	if len(it.title) > 1000 {
		it.title = it.title[:1000]
	}
	dj, _ := json.Marshal(it.detail)
	fam := hwFamily(it.kind)
	r.seen[fam]++
	res, err := r.db.Exec(`INSERT INTO twoai_health_watch (url, topic, source, title, item_date, kind, sub_hub, detail)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::date,$6,$7,$8::jsonb)
		ON CONFLICT (url) DO NOTHING`,
		it.url, it.topic, it.source, it.title, it.date, it.kind, hub, string(dj))
	if err != nil {
		r.notice("insert %s: %v", it.url, err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		r.added[fam]++
		r.byTopic[it.topic]++
		return true
	}
	return false
}

func twoaiHealthWatch(db *sql.DB) error {
	start := time.Now()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_health_watch (
		url text PRIMARY KEY,
		topic text NOT NULL DEFAULT 'diabetes',
		source text NOT NULL,
		title text NOT NULL,
		item_date date,
		kind text NOT NULL,
		sub_hub text,
		status text NOT NULL DEFAULT 'new',
		found_on date NOT NULL DEFAULT current_date,
		found_at timestamptz NOT NULL DEFAULT now(),
		detail jsonb NOT NULL DEFAULT '{}'::jsonb)`); err != nil {
		return fmt.Errorf("create twoai_health_watch: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_health_watch_topic_found ON twoai_health_watch (topic, status, found_at DESC)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_health_watch_gnews ON twoai_health_watch ((detail->>'gnews'))`)
	db.Exec(`CREATE INDEX IF NOT EXISTS twoai_health_watch_verify ON twoai_health_watch ((detail->>'verify'))`)
	// One row per topic per calendar day: the guard for the daily bridge row.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twoai_health_watch_bridge (
		topic text NOT NULL,
		sent_on date NOT NULL DEFAULT current_date,
		sent_at timestamptz NOT NULL DEFAULT now(),
		items int NOT NULL,
		bridge_topic text,
		PRIMARY KEY (topic, sent_on))`); err != nil {
		return fmt.Errorf("create twoai_health_watch_bridge: %w", err)
	}

	limit := twoaiStageDeadlineDefault
	if d, ok := twoaiStageDeadline[twoaiHealthStage]; ok {
		limit = d
	}
	r := &hwRun{
		db: db, stop: start.Add(limit - twoaiHealthReserve),
		ncbiKey: os.Getenv("NCBI_API_KEY"),
		seen:    map[string]int{}, added: map[string]int{}, byTopic: map[string]int{},
	}

	r.pubmed()
	r.trials()
	r.openFDA()
	r.feeds()
	r.coverage()
	r.verifyCoverage()

	for _, topic := range []string{"diabetes", "lpa"} {
		r.bridge(topic)
	}

	for _, n := range r.notices {
		// Stdout, not stderr: a source being down is a notice, and
		// PowerShell logs anything on stderr as a NativeCommandError.
		fmt.Printf("twoai_health_watch: notice: %s\n", n)
	}
	total := 0
	for _, n := range r.added {
		total += n
	}
	fmt.Printf("twoai_health_watch: pubmed=%d trials=%d fda=%d ema=%d journals=%d companies=%d societies=%d coverage=%d diabetes=%d lpa=%d new=%d resolved=%d notices=%d elapsed=%s ok=true\n",
		r.added["pubmed"], r.added["trials"], r.added["fda"], r.added["ema"], r.added["journals"],
		r.added["companies"], r.added["societies"], r.added["coverage"], r.byTopic["diabetes"], r.byTopic["lpa"],
		total, r.resolved, len(r.notices), time.Since(start).Round(time.Second))
	return nil
}

// ---------------------------------------------------------------------
// PubMed through NCBI E-utilities.
// ---------------------------------------------------------------------

// ncbiGet paces E-utilities calls under NCBI's limit. 350ms between calls
// drew a 429 on the third request when probed on 2026-10-05, so the gap is
// 400ms without a key, and a 429 is waited out once before it counts as a
// failure.
func (r *hwRun) ncbiGet(endpoint string, v url.Values) ([]byte, error) {
	gap := 400 * time.Millisecond // under three a second without a key
	if r.ncbiKey != "" {
		gap = 120 * time.Millisecond // under ten a second with one
		v.Set("api_key", r.ncbiKey)
	}
	v.Set("tool", twoaiHealthNCBITool)
	v.Set("email", twoaiHealthNCBIEmail)
	u := "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/" + endpoint + "?" + v.Encode()
	var body []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if w := gap - time.Since(r.lastNCBI); w > 0 {
			time.Sleep(w)
		}
		body, err = r.get(u)
		r.lastNCBI = time.Now()
		if err == nil || !strings.HasSuffix(err.Error(), ": 429") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return body, err
}

func (r *hwRun) esearch(term string, days int) ([]string, error) {
	v := url.Values{}
	v.Set("db", "pubmed")
	v.Set("term", term+twoaiHealthHumanFilter)
	v.Set("datetype", "edat")
	v.Set("reldate", fmt.Sprint(days))
	v.Set("retmode", "json")
	v.Set("retmax", "200")
	body, err := r.ncbiGet("esearch.fcgi", v)
	if err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			IDs    []string `json:"idlist"`
			Errors any      `json:"errorlist"`
		} `json:"esearchresult"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res.Result.IDs, nil
}

type hwPubmed struct {
	PMID, Title, Journal, Date, DOI string
	Types                           []string
}

// hwParsePubmedSummary reads an esummary JSON response.
func hwParsePubmedSummary(body []byte) ([]hwPubmed, error) {
	var raw struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var uids []string
	if u, ok := raw.Result["uids"]; ok {
		json.Unmarshal(u, &uids)
	}
	var out []hwPubmed
	for _, id := range uids {
		var d struct {
			Title       string   `json:"title"`
			Source      string   `json:"source"`
			FullJournal string   `json:"fulljournalname"`
			SortPubDate string   `json:"sortpubdate"`
			PubType     []string `json:"pubtype"`
			ArticleIDs  []struct {
				IDType string `json:"idtype"`
				Value  string `json:"value"`
			} `json:"articleids"`
		}
		if json.Unmarshal(raw.Result[id], &d) != nil {
			continue
		}
		p := hwPubmed{PMID: id, Title: hwClean(d.Title), Journal: d.FullJournal, Types: d.PubType}
		if p.Journal == "" {
			p.Journal = d.Source
		}
		if len(d.SortPubDate) >= 10 {
			if t, err := time.Parse("2006/01/02", d.SortPubDate[:10]); err == nil {
				p.Date = t.Format("2006-01-02")
			}
		}
		for _, a := range d.ArticleIDs {
			if a.IDType == "doi" {
				p.DOI = a.Value
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *hwRun) pubmed() {
	type hit struct {
		topic   string
		queries []string
		verify  string
	}
	hits := map[string]*hit{}
	var order []string
	add := func(id, topic, query, verify string) {
		h := hits[id]
		if h == nil {
			h = &hit{topic: topic}
			hits[id] = h
			order = append(order, id)
		}
		h.queries = append(h.queries, query)
		if verify != "" && h.verify == "" {
			h.verify = verify
		}
	}
	for _, q := range twoaiHealthPubmed {
		if r.late("PubMed " + q.key) {
			return
		}
		days := 3
		if q.topic == "lpa" {
			days = 7 // a smaller field, so a wider window costs little
		}
		ids, err := r.esearch(q.term, days)
		if err != nil {
			r.notice("PubMed %s: %v", q.key, err)
			continue
		}
		for _, id := range ids {
			add(id, q.topic, q.key, "")
		}
	}
	// Verify queries look back sixty days: a confirmation published last
	// month is still the confirmation, and the URL key keeps it to one row.
	for _, v := range twoaiHealthVerify {
		if v.pubmed == "" {
			continue
		}
		if r.late("PubMed verify " + v.key) {
			break
		}
		ids, err := r.esearch(v.pubmed, 60)
		if err != nil {
			r.notice("PubMed verify %s: %v", v.key, err)
			continue
		}
		for _, id := range ids {
			add(id, v.topic, "verify:"+v.key, v.key)
		}
	}
	// Only PMIDs not already stored need a summary.
	var fresh []string
	for _, id := range order {
		var n int
		r.db.QueryRow(`SELECT count(*) FROM twoai_health_watch WHERE url=$1`, hwPubmedURL(id)).Scan(&n)
		if n == 0 {
			fresh = append(fresh, id)
		}
	}
	for i := 0; i < len(fresh); i += 150 {
		if r.late("PubMed summaries") {
			return
		}
		j := i + 150
		if j > len(fresh) {
			j = len(fresh)
		}
		v := url.Values{}
		v.Set("db", "pubmed")
		v.Set("id", strings.Join(fresh[i:j], ","))
		v.Set("retmode", "json")
		body, err := r.ncbiGet("esummary.fcgi", v)
		if err != nil {
			r.notice("PubMed esummary: %v", err)
			continue
		}
		recs, err := hwParsePubmedSummary(body)
		if err != nil {
			r.notice("PubMed esummary parse: %v", err)
			continue
		}
		for _, p := range recs {
			h := hits[p.PMID]
			if h == nil {
				continue
			}
			d := map[string]any{"pmid": p.PMID, "journal": p.Journal, "queries": h.queries}
			if p.DOI != "" {
				d["doi"] = p.DOI
			}
			if len(p.Types) > 0 {
				d["pubtype"] = p.Types
			}
			if h.verify != "" {
				// Found by the key's own PubMed query: a candidate to read,
				// not a confirmation until someone has read it.
				d["verify"] = h.verify
				d["verify_via"] = "pubmed_query"
			}
			r.save(hwItem{url: hwPubmedURL(p.PMID), source: "PubMed", title: p.Title, date: p.Date,
				kind: "pubmed", topic: h.topic, detail: d})
		}
	}
}

func hwPubmedURL(pmid string) string { return "https://pubmed.ncbi.nlm.nih.gov/" + pmid + "/" }

// ---------------------------------------------------------------------
// ClinicalTrials.gov API v2.
// ---------------------------------------------------------------------

type hwTrial struct {
	NCT, Title, Acronym, Status, Sponsor, Updated string
	Phases, Conditions, Interventions             []string
}

const twoaiHealthTrialFields = "NCTId,BriefTitle,Acronym,Phase,OverallStatus,LeadSponsorName,LastUpdatePostDate,Condition,InterventionName"

// hwParseTrials reads one page of a v2 studies response and returns the
// next page token, empty on the last page.
func hwParseTrials(body []byte) ([]hwTrial, string, error) {
	var res struct {
		Studies []struct {
			P struct {
				ID struct {
					NCT     string `json:"nctId"`
					Title   string `json:"briefTitle"`
					Acronym string `json:"acronym"`
				} `json:"identificationModule"`
				Status struct {
					Overall string `json:"overallStatus"`
					Updated struct {
						Date string `json:"date"`
					} `json:"lastUpdatePostDateStruct"`
				} `json:"statusModule"`
				Sponsor struct {
					Lead struct {
						Name string `json:"name"`
					} `json:"leadSponsor"`
				} `json:"sponsorCollaboratorsModule"`
				Conditions struct {
					List []string `json:"conditions"`
				} `json:"conditionsModule"`
				Design struct {
					Phases []string `json:"phases"`
				} `json:"designModule"`
				Arms struct {
					Interventions []struct {
						Name string `json:"name"`
					} `json:"interventions"`
				} `json:"armsInterventionsModule"`
			} `json:"protocolSection"`
		} `json:"studies"`
		Next string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, "", err
	}
	var out []hwTrial
	for _, s := range res.Studies {
		t := hwTrial{NCT: s.P.ID.NCT, Title: strings.TrimSpace(s.P.ID.Title), Acronym: s.P.ID.Acronym,
			Status: s.P.Status.Overall, Sponsor: s.P.Sponsor.Lead.Name, Updated: s.P.Status.Updated.Date,
			Phases: s.P.Design.Phases, Conditions: s.P.Conditions.List}
		for _, iv := range s.P.Arms.Interventions {
			t.Interventions = append(t.Interventions, iv.Name)
		}
		if t.NCT != "" && t.Title != "" {
			out = append(out, t)
		}
	}
	return out, res.Next, nil
}

func (r *hwRun) trialQuery(v url.Values, label string, each func(hwTrial)) {
	v.Set("fields", twoaiHealthTrialFields)
	v.Set("pageSize", "100")
	for page := 0; page < 5; page++ {
		if r.late("ClinicalTrials.gov " + label) {
			return
		}
		body, err := r.get("https://clinicaltrials.gov/api/v2/studies?" + v.Encode())
		if err != nil {
			r.notice("ClinicalTrials.gov %s: %v", label, err)
			return
		}
		trials, next, err := hwParseTrials(body)
		if err != nil {
			r.notice("ClinicalTrials.gov %s parse: %v", label, err)
			return
		}
		for _, t := range trials {
			each(t)
		}
		if next == "" {
			return
		}
		v.Set("pageToken", next)
	}
}

func (r *hwRun) trialItem(t hwTrial, topic, u string) hwItem {
	phase := strings.Join(t.Phases, ", ")
	d := map[string]any{"nct": t.NCT, "phase": phase, "status": t.Status, "sponsor": t.Sponsor,
		"last_update": t.Updated, "conditions": t.Conditions, "interventions": t.Interventions}
	if t.Acronym != "" {
		d["acronym"] = t.Acronym
	}
	title := t.Title
	if t.Acronym != "" && !strings.Contains(title, t.Acronym) {
		title += " (" + t.Acronym + ")"
	}
	return hwItem{url: u, source: "ClinicalTrials.gov", title: title, date: t.Updated, kind: "trial", topic: topic,
		classify: strings.Join([]string{title, t.NCT, phase, strings.Join(t.Conditions, " "), strings.Join(t.Interventions, " ")}, " "),
		detail:   d}
}

func (r *hwRun) trials() {
	since := time.Now().UTC().AddDate(0, 0, -4).Format("2006-01-02")
	window := "AREA[Phase](PHASE2 OR PHASE3) AND AREA[LastUpdatePostDate]RANGE[" + since + ",MAX]"
	tracked := map[string]bool{}
	for _, id := range twoaiHealthTrackedTrials {
		tracked[id] = true
	}

	// Diabetes and obesity, phase 2 or 3, updated in the last four days.
	// Obesity is included because the incretin and next-generation
	// programmes run their obesity trials separately from their diabetes
	// ones. 36 studies matched over four days on 2026-10-05.
	v := url.Values{}
	v.Set("query.cond", "diabetes OR obesity")
	v.Set("filter.advanced", window)
	r.trialQuery(v, "diabetes", func(t hwTrial) {
		topic := "diabetes"
		if tp, _ := twoaiHealthTopicOf(t.Title+" "+strings.Join(t.Interventions, " "), []string{"lpa"}); tp != "" {
			topic = tp
		}
		r.save(r.trialItem(t, topic, "https://clinicaltrials.gov/study/"+t.NCT))
	})

	// Lp(a), phase 2 or 3, same window, except the tracked trials below.
	v = url.Values{}
	v.Set("query.cond", "lipoprotein(a)")
	v.Set("filter.advanced", window)
	// The condition search is fuzzy: on 2026-10-05 it returned a
	// statin combination trial measured on LDL-C, so a study is kept only
	// when an Lp(a) term is in its title, conditions or interventions.
	r.trialQuery(v, "lpa", func(t hwTrial) {
		if tracked[t.NCT] {
			return
		}
		text := t.Title + " " + strings.Join(t.Conditions, " ") + " " + strings.Join(t.Interventions, " ")
		tp, matched := twoaiHealthTopicOf(text, []string{"lpa"})
		if tp == "" {
			return
		}
		it := r.trialItem(t, "lpa", "https://clinicaltrials.gov/study/"+t.NCT)
		it.detail["topic_matched"] = matched
		r.save(it)
	})

	// The tracked Lp(a) outcome trials, every update a row of its own.
	v = url.Values{}
	v.Set("filter.ids", strings.Join(twoaiHealthTrackedTrials, ","))
	r.trialQuery(v, "tracked", func(t hwTrial) {
		it := r.trialItem(t, "lpa", "https://clinicaltrials.gov/study/"+t.NCT+"#update-"+t.Updated)
		it.detail["tracked"] = true
		r.save(it)
	})
}

// ---------------------------------------------------------------------
// openFDA: device clearances (510(k) and De Novo), PMA decisions, and drug
// approvals by ingredient. openFDA lags by a few weeks (last_updated was
// 2026-09-21 on 2026-10-05), so it looks back sixty days and the URL key
// keeps each decision to one row. A search with no matches is a 404 there,
// which is a quiet day, not a failure.
// ---------------------------------------------------------------------

func (r *hwRun) openFDAGet(endpoint, search string) ([]byte, bool) {
	if r.late("openFDA " + endpoint) {
		return nil, false
	}
	v := url.Values{}
	v.Set("search", search)
	v.Set("limit", "100")
	body, err := r.get("https://api.fda.gov/" + endpoint + ".json?" + v.Encode())
	if err != nil {
		if strings.HasSuffix(err.Error(), ": 404") {
			return nil, false
		}
		r.notice("openFDA %s: %v", endpoint, err)
		return nil, false
	}
	return body, true
}

func hwYMD(s string) string {
	if t, err := time.Parse("20060102", s); err == nil {
		return t.Format("2006-01-02")
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format("2006-01-02")
	}
	return ""
}

// hwDeviceURL is the FDA database page for a 510(k) or De Novo number.
func hwDeviceURL(k string) string {
	if strings.HasPrefix(k, "DEN") {
		return "https://www.accessdata.fda.gov/scripts/cdrh/cfdocs/cfpmn/denovo.cfm?id=" + k
	}
	return "https://www.accessdata.fda.gov/scripts/cdrh/cfdocs/cfpmn/pmn.cfm?ID=" + k
}

func (r *hwRun) openFDA() {
	now := time.Now().UTC()
	window := "[" + now.AddDate(0, 0, -60).Format("20060102") + " TO " + now.Format("20060102") + "]"

	// 510(k) and De Novo. 4 matched over August to October on 2026-10-05,
	// among them DEN250049, Abbott's Libre Duo glucose and ketone sensor.
	if body, ok := r.openFDAGet("device/510k", "decision_date:"+window+" AND (device_name:glucose OR device_name:insulin OR device_name:ketone OR device_name:pancreas)"); ok {
		var raw struct {
			Results []map[string]any `json:"results"`
		}
		json.Unmarshal(body, &raw)
		for _, m := range raw.Results {
			k, applicant := hwStr(m["k_number"]), hwStr(m["applicant"])
			if k == "" {
				continue
			}
			what := "510(k) clearance"
			if strings.HasPrefix(k, "DEN") {
				what = "De Novo authorisation"
			}
			title := fmt.Sprintf("FDA %s %s: %s (%s)", what, k, hwClean(hwStr(m["device_name"])), applicant)
			r.save(hwItem{url: hwDeviceURL(k), source: "FDA (openFDA devices)", title: title,
				date: hwYMD(hwStr(m["decision_date"])), kind: "fda", topic: "diabetes",
				detail: map[string]any{"number": k, "applicant": applicant,
					"decision_code": hwStr(m["decision_code"]), "product_code": hwStr(m["product_code"])}})
		}
	}

	// PMA decisions. Most are 30-day manufacturing notices, which say
	// nothing about the device a patient uses, so those are skipped.
	if body, ok := r.openFDAGet("device/pma", "decision_date:"+window+" AND (trade_name:glucose OR generic_name:glucose OR trade_name:insulin OR generic_name:insulin OR generic_name:ketone)"); ok {
		var raw struct {
			Results []map[string]any `json:"results"`
		}
		json.Unmarshal(body, &raw)
		for _, m := range raw.Results {
			pma, sup := hwStr(m["pma_number"]), hwStr(m["supplement_number"])
			stype, reason := hwStr(m["supplement_type"]), hwStr(m["supplement_reason"])
			if pma == "" || stype == "30-Day Notice" || strings.HasPrefix(reason, "Process Change") {
				continue
			}
			id := pma + sup
			title := fmt.Sprintf("FDA PMA %s: %s (%s)", id, hwClean(hwStr(m["trade_name"])), hwStr(m["applicant"]))
			if reason != "" {
				title += ", " + reason
			}
			r.save(hwItem{url: "https://www.accessdata.fda.gov/scripts/cdrh/cfdocs/cfpma/pma.cfm?id=" + id,
				source: "FDA (openFDA devices)", title: title, date: hwYMD(hwStr(m["decision_date"])), kind: "fda",
				topic: "diabetes", classify: title + " " + hwStr(m["generic_name"]),
				detail: map[string]any{"pma": pma, "supplement": sup, "supplement_type": stype, "reason": reason,
					"generic_name": hwStr(m["generic_name"]), "decision_code": hwStr(m["decision_code"])}})
		}
	}

	// Drug approvals by ingredient. Each submission in the window is its own
	// row, keyed by a fragment on the application page. Manufacturing (CMC)
	// supplements are skipped for the same reason as the PMA notices.
	terms := make([]string, len(twoaiHealthFDADrugs))
	for i, d := range twoaiHealthFDADrugs {
		terms[i] = "products.active_ingredients.name:" + d
	}
	if body, ok := r.openFDAGet("drug/drugsfda", "submissions.submission_status_date:"+window+" AND ("+strings.Join(terms, " OR ")+")"); ok {
		var res struct {
			Results []struct {
				Appl     string `json:"application_number"`
				Sponsor  string `json:"sponsor_name"`
				Products []struct {
					Brand       string `json:"brand_name"`
					Ingredients []struct {
						Name string `json:"name"`
					} `json:"active_ingredients"`
				} `json:"products"`
				Submissions []struct {
					Type   string `json:"submission_type"`
					Number string `json:"submission_number"`
					Status string `json:"submission_status"`
					Date   string `json:"submission_status_date"`
					Class  string `json:"submission_class_code_description"`
				} `json:"submissions"`
			} `json:"results"`
		}
		json.Unmarshal(body, &res)
		cut := now.AddDate(0, 0, -60).Format("20060102")
		for _, a := range res.Results {
			brands, ings := []string{}, []string{}
			for _, p := range a.Products {
				if p.Brand != "" && !hwHas(brands, p.Brand) {
					brands = append(brands, p.Brand)
				}
				for _, in := range p.Ingredients {
					if !hwHas(ings, strings.ToLower(in.Name)) {
						ings = append(ings, strings.ToLower(in.Name))
					}
				}
			}
			num := strings.TrimLeft(a.Appl, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
			for _, s := range a.Submissions {
				if s.Date < cut || (s.Status != "AP" && s.Status != "TA") || s.Class == "Manufacturing (CMC)" {
					continue
				}
				if strings.HasPrefix(a.Appl, "ANDA") && s.Type == "SUPPL" {
					continue // generic supplements are paperwork
				}
				what := "approval"
				if s.Status == "TA" {
					what = "tentative approval"
				}
				label := s.Type + " " + s.Number
				if s.Class != "" {
					label += ", " + s.Class
				}
				title := fmt.Sprintf("FDA %s, %s %s (%s): %s", what, a.Appl, strings.Join(brands, " / "),
					strings.Join(ings, ", "), label)
				topic, _ := twoaiHealthTopicOf(strings.Join(ings, " "), nil)
				if topic == "" {
					topic = "diabetes"
				}
				r.save(hwItem{url: "https://www.accessdata.fda.gov/scripts/cder/daf/index.cfm?event=overview.process&ApplNo=" + num + "#" + s.Type + "-" + s.Number,
					source: "FDA (openFDA drugs)", title: title, date: hwYMD(s.Date), kind: "fda", topic: topic,
					detail: map[string]any{"application": a.Appl, "sponsor": a.Sponsor, "brands": brands, "ingredients": ings,
						"submission": label, "status": s.Status}})
			}
		}
	}
}

func hwStr(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------
// RSS and Atom feeds.
// ---------------------------------------------------------------------

type hwFeedItem struct {
	twoaiFeedItem
	Source string `xml:"source"` // Google News names the publisher here
}

// hwFeedDoc reads RSS 2.0 (items in the channel), RSS 1.0 as NEJM and the
// Lancet publish it (items beside the channel), and Atom.
type hwFeedDoc struct {
	Channel struct {
		Items []hwFeedItem `xml:"item"`
	} `xml:"channel"`
	Items   []hwFeedItem `xml:"item"`
	Entries []hwFeedItem `xml:"entry"`
}

func hwParseFeed(body []byte) ([]hwFeedItem, error) {
	var doc hwFeedDoc
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	// The same single-byte fallback as the agency watch: these feeds are
	// ASCII with the odd smart quote.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "windows-1252", "iso-8859-1", "latin1", "us-ascii":
			return input, nil
		}
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	out := append([]hwFeedItem{}, doc.Channel.Items...)
	out = append(out, doc.Items...)
	return append(out, doc.Entries...), nil
}

// hwClean turns a feed or API title into one line of plain text.
func hwClean(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(stripTags(html.UnescapeString(s)))), " ")
}

var hwEMARevision = regexp.MustCompile(`Revision: (\d+)`)

// hwEMAKey gives each EMA medicine event its own row. The EPAR feeds link
// every update of a medicine to the same page, so the revision number, or
// the opinion or withdrawal status, goes into a fragment on the link.
func hwEMAKey(link, title string) string {
	if !strings.Contains(link, "/EPAR/") {
		return link
	}
	if m := hwEMARevision.FindStringSubmatch(title); m != nil {
		return link + "#revision-" + m[1]
	}
	switch {
	case strings.Contains(title, "Status: Opinion"):
		return link + "#opinion"
	case strings.Contains(title, "withdrawn") || strings.Contains(title, "Withdrawn"):
		return link + "#withdrawn"
	}
	return link + "#authorised"
}

func (r *hwRun) feeds() {
	for _, f := range twoaiHealthFeeds {
		if r.late(f.source + " feed") {
			return
		}
		body, err := r.get(f.url)
		if err != nil {
			r.notice("%s feed: %v", f.source, err)
			continue
		}
		items, err := hwParseFeed(body)
		if err != nil {
			r.notice("%s feed parse: %v", f.source, err)
			continue
		}
		for _, it := range items {
			link := it.URL()
			title := hwClean(it.Title)
			summary := twoaiFeedSummary(it.Description, it.Summary, it.Content)
			if link == "" || title == "" {
				continue
			}
			topic, matched := twoaiHealthTopicOf(title+" "+summary, f.topics)
			if topic == "" {
				if f.filter {
					continue
				}
				topic = f.topics[0]
			}
			if f.kind == "ema" {
				link = hwEMAKey(link, title)
			}
			d := map[string]any{"feed": f.url}
			if matched != "" {
				d["topic_matched"] = matched
			}
			if summary != "" {
				d["summary"] = summary
			}
			r.save(hwItem{url: link, source: f.source, title: title,
				date: twoaiFeedDate(it.PubDate, it.Published, it.Updated, it.Date),
				kind: f.kind, topic: topic, classify: title + " " + summary, detail: d})
		}
	}
}

// ---------------------------------------------------------------------
// Google News coverage, for newsrooms with no working feed and for the
// verify list. In the style of the intel watch's coverage feeds.
// ---------------------------------------------------------------------

func hwGNewsURL(query, window string) string {
	return "https://news.google.com/rss/search?q=" + url.QueryEscape(query+" when:"+window) + "&hl=en-US&gl=US&ceid=US:en"
}

// hwGNewsTitle drops the " - Publisher" suffix Google News adds to titles.
func hwGNewsTitle(title, publisher string) string {
	title = hwClean(title)
	if publisher != "" {
		title = strings.TrimSuffix(title, " - "+strings.TrimSpace(publisher))
	}
	return strings.TrimSpace(title)
}

// gnewsItems fetches a coverage query and returns its items newest first,
// at most max of them.
func (r *hwRun) gnewsItems(label, query, window string, max int) []hwFeedItem {
	body, err := r.get(hwGNewsURL(query, window))
	if err != nil {
		r.notice("%s coverage: %v", label, err)
		return nil
	}
	items, err := hwParseFeed(body)
	if err != nil {
		r.notice("%s coverage parse: %v", label, err)
		return nil
	}
	sort.SliceStable(items, func(i, j int) bool {
		return twoaiFeedDate(items[i].PubDate) > twoaiFeedDate(items[j].PubDate)
	})
	if len(items) > max {
		items = items[:max]
	}
	return items
}

// saveCoverage stores one Google News item under its publisher URL when the
// redirect resolves within budget, and under the Google link otherwise. The
// Google link is kept in detail->>'gnews' either way, which is what stops a
// story already stored from being resolved and stored a second time.
func (r *hwRun) saveCoverage(it hwFeedItem, source, topic, title string, d map[string]any) {
	glink := it.URL()
	if glink == "" || title == "" {
		return
	}
	var n int
	r.db.QueryRow(`SELECT count(*) FROM twoai_health_watch WHERE url=$1 OR detail->>'gnews'=$1`, glink).Scan(&n)
	if n > 0 {
		r.seen["coverage"]++
		return
	}
	u := glink
	if r.resolved < twoaiHealthResolveBudget && !r.late("coverage resolution") {
		r.resolved++
		u = resolveGoogleNews(glink)
	}
	d["gnews"] = glink
	d["publisher"] = strings.TrimSpace(it.Source)
	r.save(hwItem{url: u, source: source, title: title, date: twoaiFeedDate(it.PubDate),
		kind: "coverage", topic: topic, detail: d})
}

func (r *hwRun) coverage() {
	for _, c := range twoaiHealthCoverage {
		if r.late(c.source + " coverage") {
			return
		}
		for _, it := range r.gnewsItems(c.source, c.query, c.window, 15) {
			title := hwGNewsTitle(it.Title, it.Source)
			topic, matched := twoaiHealthTopicOf(title, c.topics)
			if topic == "" {
				if c.gate {
					continue
				}
				topic = c.topics[0]
			}
			d := map[string]any{"query": c.query}
			if matched != "" {
				d["topic_matched"] = matched
			}
			r.saveCoverage(it, c.source+" (coverage)", topic, title, d)
		}
	}
}

func (r *hwRun) verifyCoverage() {
	for _, v := range twoaiHealthVerify {
		if v.gnews == "" {
			continue
		}
		if r.late("verify coverage " + v.key) {
			return
		}
		// Google News answers a query with whatever is nearest, which on
		// 2026-10-05 included a Thermo Fisher marketing piece for the
		// ketone monitor query. A story is kept only when the key's own
		// pattern matches its headline, so a verify tag means something.
		for _, it := range r.gnewsItems("verify "+v.key, v.gnews, "30d", 10) {
			title := hwGNewsTitle(it.Title, it.Source)
			m := v.re.FindString(title)
			if m == "" {
				continue
			}
			pub := strings.TrimSpace(it.Source)
			if pub == "" {
				pub = "News"
			}
			r.saveCoverage(it, pub+" (coverage)", v.topic, title,
				map[string]any{"query": v.gnews, "verify": v.key, "verify_matched": m, "verify_via": "pattern"})
		}
	}
}

// ---------------------------------------------------------------------
// The daily bridge row, one per topic.
// ---------------------------------------------------------------------

type hwCount struct {
	name string
	n    int
}

type hwRow struct {
	kind, title, source, date, url, verify string
}

var twoaiHealthLabels = map[string]string{"diabetes": "Diabetes watch", "lpa": "Lp(a) watch"}

// hwBridgeBody writes the bridge message. It stays near forty lines
// whatever the day looks like: the counts are one line each and the item
// list is cut to fit.
func hwBridgeBody(label, today, since string, n int, hubs, sources, verify []hwCount, rows []hwRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s: %d new items since %s.\n", label, today, n, since)
	b.WriteString("All items are primary sources (PubMed, ClinicalTrials.gov, FDA, EMA, journal feeds, company and society newsrooms) except those marked coverage, which are news reports found through Google News and need a primary source before anything is published.\n")
	join := func(cs []hwCount, max int) string {
		parts := []string{}
		rest := 0
		for i, c := range cs {
			if i < max {
				parts = append(parts, fmt.Sprintf("%s %d", c.name, c.n))
			} else {
				rest += c.n
			}
		}
		if rest > 0 {
			parts = append(parts, fmt.Sprintf("others %d", rest))
		}
		return strings.Join(parts, ", ")
	}
	fmt.Fprintf(&b, "By sub-hub: %s.\n", join(hubs, 12))
	fmt.Fprintf(&b, "By source: %s.\n", join(sources, 12))
	if len(verify) > 0 {
		fmt.Fprintf(&b, "Verify list hits (detail->>'verify'): %s.\n", join(verify, 12))
	}
	b.WriteString("Newest:\n")
	for i, r := range rows {
		if i == 32 {
			break
		}
		t := r.title
		if len(t) > 160 {
			t = t[:157] + "..."
		}
		date := r.date
		if date == "" {
			date = "undated"
		}
		tag := ""
		if r.kind == "coverage" {
			tag = "[coverage] "
		}
		if r.verify != "" {
			tag += "[verify " + r.verify + "] "
		}
		fmt.Fprintf(&b, "- %s%s (%s, %s) %s\n", tag, t, r.source, date, r.url)
	}
	if len(rows) > 32 {
		fmt.Fprintf(&b, "... and %d more.\n", n-32)
	}
	b.WriteString("All rows are in twoai_health_watch, status new until you change it. srj owns the stage, send code needs by bridge.")
	return b.String()
}

func (r *hwRun) counts(q string, args ...any) []hwCount {
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []hwCount
	for rows.Next() {
		var c hwCount
		if rows.Scan(&c.name, &c.n) == nil {
			out = append(out, c)
		}
	}
	return out
}

func (r *hwRun) bridge(topic string) {
	label := twoaiHealthLabels[topic]
	var sentToday bool
	r.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM twoai_health_watch_bridge WHERE topic=$1 AND sent_on=current_date)`, topic).Scan(&sentToday)
	if sentToday {
		return
	}
	var last sql.NullTime
	r.db.QueryRow(`SELECT max(sent_at) FROM twoai_health_watch_bridge WHERE topic=$1`, topic).Scan(&last)
	since := "the first run"
	cutoff := time.Time{}
	if last.Valid {
		cutoff = last.Time
		since = "the last bridge row, " + last.Time.UTC().Format("2006-01-02 15:04 UTC")
	}
	const scope = `FROM twoai_health_watch WHERE topic=$1 AND status='new' AND found_at > $2`
	var n int
	if err := r.db.QueryRow(`SELECT count(*) `+scope, topic, cutoff).Scan(&n); err != nil || n == 0 {
		return
	}
	hubs := r.counts(`SELECT COALESCE(sub_hub,'none'), count(*) `+scope+` GROUP BY 1 ORDER BY 2 DESC, 1`, topic, cutoff)
	sources := r.counts(`SELECT source, count(*) `+scope+` GROUP BY 1 ORDER BY 2 DESC, 1`, topic, cutoff)
	verify := r.counts(`SELECT detail->>'verify', count(*) `+scope+` AND detail ? 'verify' GROUP BY 1 ORDER BY 2 DESC, 1`, topic, cutoff)
	var list []hwRow
	if rows, err := r.db.Query(`SELECT kind, title, source, COALESCE(item_date::text,''), url, COALESCE(detail->>'verify','') `+scope+`
		ORDER BY item_date DESC NULLS LAST, found_at DESC LIMIT 33`, topic, cutoff); err == nil {
		for rows.Next() {
			var x hwRow
			if rows.Scan(&x.kind, &x.title, &x.source, &x.date, &x.url, &x.verify) == nil {
				list = append(list, x)
			}
		}
		rows.Close()
	}
	subject := fmt.Sprintf("%s: %d new items", label, n)
	body := hwBridgeBody(label, time.Now().UTC().Format("2006-01-02"), since, n, hubs, sources, verify, list)
	if _, err := r.db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`, subject, body); err != nil {
		r.notice("bridge row for %s: %v", topic, err)
		return
	}
	r.db.Exec(`INSERT INTO twoai_health_watch_bridge (topic, items, bridge_topic) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, topic, n, subject)
	fmt.Printf("twoai_health_watch: bridge row sent, %s\n", subject)
}
