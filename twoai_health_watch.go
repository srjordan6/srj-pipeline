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
// NOISE IS SKIPPED AT HARVEST (bridge row 496). The content project reviewed
// the first 391 rows and asked that FDA labelling supplements, generic
// approvals, journal corrections, letters and issue furniture never reach
// the daily list. Those rows are still stored, so a URL is never fetched
// twice, but at status 'skipped' with detail->>'skip_reason' and the words
// that fired in detail->>'skip_matched'.
//
// COVERAGE IS RESOLVED TO ITS PRIMARY (bridge row 496). Each run takes up to
// thirty coverage rows and looks for the company release, regulator record,
// registry entry or paper behind the story: first among the newsroom items
// and stored primaries dated from fourteen days before the story to four
// after, then through the links on the publisher's page. A candidate is accepted only when its title shares
// a drug or product name and one more distinctive term with the coverage
// headline. The primary is filed as its own row with
// detail->>'resolved_from', and the coverage row goes to status 'resolved'
// with detail->>'primary_url'. A row gets three tries, twelve hours apart,
// before it is left at 'needs primary' and named in the daily bridge row.
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
	"net/http"
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
	// Coverage resolution: rows tried per run, page fetches that work may
	// spend per run, links followed from one publisher page, and the tries
	// a row gets, at least twelve hours apart, before it is given up.
	twoaiHealthResolveRows    = 30
	twoaiHealthResolveFetches = 120
	twoaiHealthResolveLinks   = 4
	twoaiHealthResolveTries   = 3
	twoaiHealthResolveGap     = 12 * time.Hour
	// Publisher and company pages are fetched as a browser-compatible client
	// that still names itself, because many news sites refuse a bare bot UA.
	twoaiHealthPageUA = "Mozilla/5.0 (compatible; srj-pipeline health watch; +https://srjconsultingservices.com)"
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
// Noise skipped at harvest. Calibrated on 2026-10-05 against the 391 rows
// the content project reviewed (bridge row 496): every rule below fires on
// rows it skipped, and none fires on any of the 44 rows it used, which
// TestHwSkipReasonNeverSkipsUsed keeps true.
// ---------------------------------------------------------------------

// hwSkipTitles are title shapes that are not articles worth reading: notices
// about other papers, letters and replies, and the furniture of an issue.
// They apply to journal feed items and PubMed records only, so a company
// release such as Lilly's "An open letter ..." is never caught.
var hwSkipTitles = []struct {
	reason string
	re     *regexp.Regexp
}{
	{"correction or retraction notice", regexp.MustCompile(`(?i)^(correction|corrigendum|erratum|errata)(\s*(to\b|:|\.|-|\[)|\s*$)|^(retraction|retracted|notice of retraction|expression of concern)\b`)},
	{"letter, comment or reply", regexp.MustCompile(`(?i)^(reply|in reply|authors?'? reply|response to (comment|letter)s?|comments? on|letter to the editor|letter)\b|\breply to\b.*\[letter\]|\[letter\]\s*$|\.\s*reply\.?\s*$`)},
	{"not an article", regexp.MustCompile(`(?i)^(about the (artist|editor|cover)\b|(on the )?cover( (image|art|illustration|page|story))?\s*(:|$)|in this issue\b|table of contents\b|issues and events\s*$|up front\s*$|masthead\s*$|editorial board\s*$|reviewer acknowledg)`)},
}

// hwSkipSections are the Lancet's own section labels, the "[Comment]" that
// opens each feed title, that mark an item as not an article.
var hwSkipSections = map[string]string{
	"comment":               "letter, comment or reply",
	"correspondence":        "letter, comment or reply",
	"editorial":             "editorial",
	"corrections":           "correction or retraction notice",
	"correction":            "correction or retraction notice",
	"department of error":   "correction or retraction notice",
	"retraction":            "correction or retraction notice",
	"expression of concern": "correction or retraction notice",
	"in focus":              "not an article",
	"world report":          "not an article",
	"perspectives":          "not an article",
	"obituary":              "not an article",
}

// hwSkipPubtypes are the PubMed publication types that mark a record as not
// an article. Editorial is deliberately absent: PubMed files invited
// commentary under it, and the content project used one such record,
// "Lipoprotein(a) After HORIZON", on the Lp(a) hub.
var hwSkipPubtypes = map[string]string{
	"Letter":                    "letter, comment or reply",
	"Comment":                   "letter, comment or reply",
	"Published Erratum":         "correction or retraction notice",
	"Retraction of Publication": "correction or retraction notice",
	"Retracted Publication":     "correction or retraction notice",
	"Expression of Concern":     "correction or retraction notice",
}

// NEJM's DOI says what an item is: NEJMoa an original article, NEJMc a
// letter, NEJMe an editorial, NEJMx a correction, NEJMicm an image.
var hwNEJMTypeRe = regexp.MustCompile(`(?i)/10\.1056/(NEJM(?:c|e|x|icm))\d`)

var hwNEJMSkip = map[string]string{
	"nejmc":   "letter, comment or reply",
	"nejme":   "editorial",
	"nejmx":   "correction or retraction notice",
	"nejmicm": "not an article",
}

var hwSectionRe = regexp.MustCompile(`^\[([A-Za-z][A-Za-z '&-]{1,40})\]\s*(.+)$`)

// hwFeedSection splits a leading section label, "[Comment] Title", off a
// feed title. A title without one comes back whole with an empty section.
func hwFeedSection(title string) (string, string) {
	if m := hwSectionRe.FindStringSubmatch(title); m != nil {
		return m[1], strings.TrimSpace(m[2])
	}
	return "", title
}

// hwStrings reads a detail value that is a list of strings, as built in
// Go or as read back from JSON.
func hwStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// hwSkipReason says why an item should be stored as skipped, and what it
// matched on, or returns two empty strings for an item worth reading.
func hwSkipReason(kind, title, link string, d map[string]any) (string, string) {
	switch kind {
	case "fda":
		if appl := hwStr(d["application"]); strings.HasPrefix(appl, "ANDA") {
			return "ANDA generic approval", appl
		}
		for _, k := range []string{"submission_class", "submission_class_code"} {
			if c := hwStr(d[k]); strings.HasPrefix(strings.ToLower(c), "labeling") {
				return "FDA labelling-only supplement", c
			}
		}
		if hwStr(d["status"]) == "TA" {
			// Every tentative approval in the first runs was a generic or a
			// new dosage form held back by patents, and none can be sold.
			return "FDA tentative approval", "TA"
		}
	case "journal", "pubmed":
		if sec := hwStr(d["section"]); sec != "" {
			if why := hwSkipSections[strings.ToLower(sec)]; why != "" {
				return why, "[" + sec + "]"
			}
		}
		if m := hwNEJMTypeRe.FindStringSubmatch(link); m != nil {
			if why := hwNEJMSkip[strings.ToLower(m[1])]; why != "" {
				return why, m[1]
			}
		}
		for _, pt := range hwStrings(d["pubtype"]) {
			if why := hwSkipPubtypes[pt]; why != "" {
				return why, "pubtype " + pt
			}
		}
		for _, s := range hwSkipTitles {
			if m := s.re.FindString(title); m != "" {
				return s.reason, strings.TrimSpace(m)
			}
		}
	}
	return "", ""
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
	skipped  map[string]int
	byTopic  map[string]int
	notices  []string
	stopped  bool
	// Coverage resolution: every item the newsroom, society and regulator
	// feeds carried this run, filtered or not, the page fetches spent, and
	// the rows resolved and given up.
	newsroom    []hwCand
	fetches     int
	covResolved int
	covGaveUp   int
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
	status := "new"
	if why, m := hwSkipReason(it.kind, it.title, it.url, it.detail); why != "" {
		status = "skipped"
		it.detail["skip_reason"] = why
		it.detail["skip_matched"] = m
	}
	if len(it.title) > 1000 {
		it.title = it.title[:1000]
	}
	dj, _ := json.Marshal(it.detail)
	fam := hwFamily(it.kind)
	r.seen[fam]++
	res, err := r.db.Exec(`INSERT INTO twoai_health_watch (url, topic, source, title, item_date, kind, sub_hub, status, detail)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::date,$6,$7,$8,$9::jsonb)
		ON CONFLICT (url) DO NOTHING`,
		it.url, it.topic, it.source, it.title, it.date, it.kind, hub, status, string(dj))
	if err != nil {
		r.notice("insert %s: %v", it.url, err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if status == "skipped" {
			r.skipped[fam]++
			return true
		}
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
		seen:    map[string]int{}, added: map[string]int{}, skipped: map[string]int{}, byTopic: map[string]int{},
	}

	r.pubmed()
	r.trials()
	r.openFDA()
	r.feeds()
	r.coverage()
	r.verifyCoverage()
	r.resolveCoverage()

	for _, topic := range []string{"diabetes", "lpa"} {
		r.bridge(topic)
	}

	for _, n := range r.notices {
		// Stdout, not stderr: a source being down is a notice, and
		// PowerShell logs anything on stderr as a NativeCommandError.
		fmt.Printf("twoai_health_watch: notice: %s\n", n)
	}
	total, skipped := 0, 0
	for _, n := range r.added {
		total += n
	}
	for _, n := range r.skipped {
		skipped += n
	}
	fmt.Printf("twoai_health_watch: pubmed=%d trials=%d fda=%d ema=%d journals=%d companies=%d societies=%d coverage=%d diabetes=%d lpa=%d new=%d skipped=%d resolved=%d primaries_found=%d primaries_given_up=%d notices=%d elapsed=%s ok=true\n",
		r.added["pubmed"], r.added["trials"], r.added["fda"], r.added["ema"], r.added["journals"],
		r.added["companies"], r.added["societies"], r.added["coverage"], r.byTopic["diabetes"], r.byTopic["lpa"],
		total, skipped, r.resolved, r.covResolved, r.covGaveUp, len(r.notices), time.Since(start).Round(time.Second))
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
	// supplements are dropped for the same reason as the PMA notices.
	// Labelling supplements, generic (ANDA) approvals and tentative
	// approvals are stored at status 'skipped' by hwSkipReason.
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
					Type      string `json:"submission_type"`
					Number    string `json:"submission_number"`
					Status    string `json:"submission_status"`
					Date      string `json:"submission_status_date"`
					Class     string `json:"submission_class_code_description"`
					ClassCode string `json:"submission_class_code"`
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
						"submission": label, "status": s.Status,
						"submission_class": s.Class, "submission_class_code": s.ClassCode}})
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
			section, title := hwFeedSection(hwClean(it.Title))
			summary := twoaiFeedSummary(it.Description, it.Summary, it.Content)
			if link == "" || title == "" {
				continue
			}
			if f.kind == "ema" {
				link = hwEMAKey(link, title)
			}
			date := twoaiFeedDate(it.PubDate, it.Published, it.Updated, it.Date)
			if f.kind != "journal" {
				// Kept before the topic filter: a release the filter drops
				// can still be the primary behind a coverage story.
				r.newsroom = append(r.newsroom, hwCand{url: link, title: title, source: f.source,
					kind: f.kind, date: date, via: "newsroom feed"})
			}
			topic, matched := twoaiHealthTopicOf(title+" "+summary, f.topics)
			if topic == "" {
				if f.filter {
					continue
				}
				topic = f.topics[0]
			}
			d := map[string]any{"feed": f.url}
			if matched != "" {
				d["topic_matched"] = matched
			}
			if section != "" {
				d["section"] = section
			}
			if summary != "" {
				d["summary"] = summary
			}
			r.save(hwItem{url: link, source: f.source, title: title, date: date,
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
// Coverage resolved to its primary source.
// ---------------------------------------------------------------------

// hwDomain is one host whose pages count as a primary source, with the
// owner named as the row's source and the row kind it is filed under.
// Kind "wire" is a newswire carrying a company's release word for word,
// filed as kind company.
type hwDomain struct{ domain, owner, kind string }

// twoaiHealthPrimaryDomains maps hosts to owners. A host matches its domain
// or any subdomain, so lilly.com covers investor.lilly.com. The companies
// are those the watch follows plus their partners in the same programmes
// (Boehringer runs survodutide with Zealand, Roche petrelintide).
var twoaiHealthPrimaryDomains = []hwDomain{
	{"novonordisk.com", "Novo Nordisk", "company"},
	{"novonordisk-us.com", "Novo Nordisk", "company"},
	{"lilly.com", "Eli Lilly", "company"},
	{"sanofi.com", "Sanofi", "company"},
	{"sanofi.us", "Sanofi", "company"},
	{"insulet.com", "Insulet", "company"},
	{"omnipod.com", "Insulet", "company"},
	{"dexcom.com", "Dexcom", "company"},
	{"zealandpharma.com", "Zealand Pharma", "company"},
	{"novartis.com", "Novartis", "company"},
	{"amgen.com", "Amgen", "company"},
	{"ionis.com", "Ionis", "company"},
	{"abbott.com", "Abbott", "company"},
	{"abbott.mediaroom.com", "Abbott", "company"},
	{"medtronic.com", "Medtronic", "company"},
	{"minimed.com", "MiniMed (Medtronic Diabetes)", "company"},
	{"vrtx.com", "Vertex", "company"},
	{"mannkindcorp.com", "MannKind", "company"},
	{"crisprtx.com", "CRISPR Therapeutics", "company"},
	{"silence-therapeutics.com", "Silence Therapeutics", "company"},
	{"boehringer-ingelheim.com", "Boehringer Ingelheim", "company"},
	{"roche.com", "Roche", "company"},
	{"astrazeneca.com", "AstraZeneca", "company"},
	{"merck.com", "Merck", "company"},
	{"globenewswire.com", "GlobeNewswire", "wire"},
	{"prnewswire.com", "PR Newswire", "wire"},
	{"businesswire.com", "Business Wire", "wire"},
	{"heart.org", "American Heart Association", "society"},
	{"acc.org", "ACC", "society"},
	{"escardio.org", "ESC", "society"},
	{"eas-society.org", "European Atherosclerosis Society", "society"},
	{"diabetes.org", "American Diabetes Association", "society"},
	{"easd.org", "EASD", "society"},
	{"fda.gov", "FDA", "fda"},
	{"ema.europa.eu", "EMA", "ema"},
	{"clinicaltrials.gov", "ClinicalTrials.gov", "trial"},
	{"sec.gov", "SEC EDGAR", "company"},
	{"pubmed.ncbi.nlm.nih.gov", "PubMed", "pubmed"},
	{"doi.org", "DOI", "journal"},
	{"nejm.org", "NEJM", "journal"},
	{"thelancet.com", "The Lancet", "journal"},
	{"diabetesjournals.org", "ADA journals", "journal"},
	{"jamanetwork.com", "JAMA Network", "journal"},
	{"nature.com", "Nature", "journal"},
	{"ahajournals.org", "AHA journals", "journal"},
	{"jacc.org", "JACC", "journal"},
	{"link.springer.com", "Springer", "journal"},
	{"bmj.com", "BMJ", "journal"},
	{"academic.oup.com", "Oxford Academic", "journal"},
}

func hwPrimaryDomain(host string) (hwDomain, bool) {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	for _, d := range twoaiHealthPrimaryDomains {
		if host == d.domain || strings.HasSuffix(host, "."+d.domain) {
			return d, true
		}
	}
	return hwDomain{}, false
}

// hwRowKind is the twoai_health_watch kind a primary domain files under.
func hwRowKind(d hwDomain) string {
	if d.kind == "wire" {
		return "company"
	}
	return d.kind
}

var (
	hwHrefRe    = regexp.MustCompile(`(?is)<a\s[^>]*?href\s*=\s*["']([^"'#\s]+)`)
	hwNCTRe     = regexp.MustCompile(`\bNCT\d{8}\b`)
	hwDOIRe     = regexp.MustCompile(`\b10\.\d{4,9}/[-._;()/:A-Za-z0-9]+`)
	hwPMIDRe    = regexp.MustCompile(`pubmed\.ncbi\.nlm\.nih\.gov/(\d+)`)
	hwJunkPath  = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|svg|webp|ico|css|js|woff2?|xml|rss|pdf)$|/(tracker|share|sharer|login|signin|subscribe|search|privacy|cookies?|contact|terms|careers|feed)\b`)
	hwOGTitleRe = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<meta[^>]+(?:property|name)\s*=\s*["'](?:og:title|twitter:title|citation_title)["'][^>]*?content\s*=\s*["']([^"']+)["']`),
		regexp.MustCompile(`(?is)<meta[^>]+content\s*=\s*["']([^"']+)["'][^>]*?(?:property|name)\s*=\s*["'](?:og:title|twitter:title|citation_title)["']`),
		regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`),
	}
	hwHeadingRe = regexp.MustCompile(`(?is)<h[12][^>]*>(.*?)</h[12]>`)
	hwPubDateRe = regexp.MustCompile(`(?is)(?:article:published_time["'][^>]*?content\s*=\s*["']|"datePublished"\s*:\s*"|citation_publication_date["'][^>]*?content\s*=\s*["'])(\d{4})[-/](\d{2})[-/](\d{2})`)
)

// hwPrimaryURL decides whether a link is a primary source, and gives its
// canonical form: a DOI as doi.org, a trial as its ClinicalTrials.gov study
// page and a PubMed record as its plain PMID page, so a primary found this
// way lands on the same row the other harvesters write. Home pages, short
// navigation paths, assets and share links are refused, and a wire link
// must be a release.
func hwPrimaryURL(raw string) (string, hwDomain, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", hwDomain{}, false
	}
	d, ok := hwPrimaryDomain(u.Hostname())
	if !ok {
		return "", hwDomain{}, false
	}
	full := u.String()
	path := strings.Trim(u.Path, "/")
	if hwJunkPath.MatchString("/" + path) {
		return "", hwDomain{}, false
	}
	switch {
	case d.domain == "clinicaltrials.gov":
		if m := hwNCTRe.FindString(full); m != "" {
			return "https://clinicaltrials.gov/study/" + m, d, true
		}
		return "", hwDomain{}, false
	case d.domain == "doi.org":
		if m := hwDOIRe.FindString(path); m != "" {
			return "https://doi.org/" + strings.TrimRight(m, ".,;)"), d, true
		}
		return "", hwDomain{}, false
	case d.domain == "pubmed.ncbi.nlm.nih.gov":
		if m := hwPMIDRe.FindStringSubmatch(full); m != nil {
			return hwPubmedURL(m[1]), d, true
		}
		return "", hwDomain{}, false
	case d.kind == "wire":
		lp := strings.ToLower(path)
		if !strings.Contains(lp, "news-release") && !strings.HasPrefix(lp, "news/home/") {
			return "", hwDomain{}, false
		}
	case d.kind == "fda" || d.kind == "ema":
		if len(path) < 3 {
			return "", hwDomain{}, false
		}
	default:
		// Company, society and journal pages: an article has a long path
		// or an id, a section page a short word or two.
		if len(path) < 10 && !strings.ContainsAny(path, "0123456789") {
			return "", hwDomain{}, false
		}
	}
	u.Fragment = ""
	return u.String(), d, true
}

// hwPrimaryLinks returns the primary-source links on a publisher page, best
// first and at most max of them: regulators, companies, wires and societies
// before registries and journals, the SEC last. Links back to the page's
// own host are left out, and trial ids and DOIs written in the text count
// as links.
func hwPrimaryLinks(page, base string, max int) []string {
	bu, _ := url.Parse(base)
	type link struct {
		u    string
		rank int
	}
	var out []link
	seen := map[string]bool{}
	add := func(raw string) {
		pu, d, ok := hwPrimaryURL(raw)
		if !ok || seen[pu] {
			return
		}
		if bu != nil {
			if lu, err := url.Parse(pu); err == nil && strings.EqualFold(lu.Hostname(), bu.Hostname()) {
				return
			}
		}
		seen[pu] = true
		rank := 0
		switch {
		case d.owner == "SEC EDGAR":
			rank = 2
		case d.kind == "trial" || d.kind == "pubmed" || d.kind == "journal":
			rank = 1
		}
		out = append(out, link{pu, rank})
	}
	for _, m := range hwHrefRe.FindAllStringSubmatch(page, -1) {
		href := html.UnescapeString(m[1])
		if bu != nil {
			if ru, err := bu.Parse(href); err == nil {
				href = ru.String()
			}
		}
		add(href)
	}
	text := stripTags(page)
	for _, m := range hwNCTRe.FindAllString(text, -1) {
		add("https://clinicaltrials.gov/study/" + m)
	}
	for _, m := range hwDOIRe.FindAllString(text, -1) {
		add("https://doi.org/" + m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	var urls []string
	for _, l := range out {
		if len(urls) == max {
			break
		}
		urls = append(urls, l.u)
	}
	return urls
}

// hwPageTitle reads a page's own title, preferring og:title, and drops a
// short trailing " | Site name" when what is left still reads as a title.
// A title of three words or fewer is a template's ("News Details" on every
// novonordisk.com release, probed 2026-10-05), and then the first heading
// of five words or more stands in for it.
func hwPageTitle(page string) string {
	t := ""
	for _, re := range hwOGTitleRe {
		if m := re.FindStringSubmatch(page); m != nil {
			if t = hwClean(m[1]); t != "" {
				break
			}
		}
	}
	if len(strings.Fields(t)) <= 3 {
		for _, m := range hwHeadingRe.FindAllStringSubmatch(page, 20) {
			if h := hwClean(m[1]); len(strings.Fields(h)) >= 5 {
				t = h
				break
			}
		}
	}
	for _, sep := range []string{" | ", " – ", " - "} {
		if i := strings.LastIndex(t, sep); i >= 20 && len(t)-i-len(sep) <= 40 {
			t = strings.TrimSpace(t[:i])
		}
	}
	return t
}

// hwPageDate reads a page's publication date from its metadata, or "".
func hwPageDate(page string) string {
	if m := hwPubDateRe.FindStringSubmatch(page); m != nil {
		return hwYMD(m[1] + "-" + m[2] + "-" + m[3])
	}
	return ""
}

// ---------------------------------------------------------------------
// Title matching. A candidate is the primary behind a coverage story only
// when the two titles share a named drug or product and one more
// distinctive term, or two named drugs, or a class word (insulin, GLP-1,
// ketone) and two more distinctive terms. Company names, topic words and
// news verbs do not count, so "Insulet stock gains" never matches "Insulet
// announces results date".
// ---------------------------------------------------------------------

// hwDrugTerms are named drugs, brands, devices and trial-stage codes.
var hwDrugTerms = map[string]bool{}

// hwClassTerms are drug and device classes: shared, they say the two titles
// are about the same kind of thing, not the same thing.
var hwClassTerms = map[string]bool{}

// hwStopTerms carry no identity: function words, news verbs, the topic
// words every item shares, and the company names.
var hwStopTerms = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`semaglutide tirzepatide liraglutide dulaglutide exenatide lixisenatide
		retatrutide orforglipron cagrilintide cagrisema amycretin survodutide mazdutide petrelintide dapiglutide
		maridebart cafraglutide maritide ecnoglutide pemvidutide nisotirostide pelacarsen olpasiran lepodisiran
		zerlasiran muvalaplin enlicitide inclisiran evolocumab alirocumab obicetrapib teplizumab tzield zimislecel
		donislecel efsitora onswik icodec awiqli icosema iglarlixi denecimig mim8 afrezza technosphere ozempic
		wegovy rybelsus mounjaro zepbound foundayo saxenda victoza trulicity jardiance farxiga invokana kerendia
		finerenone empagliflozin dapagliflozin canagliflozin sotagliflozin ertugliflozin omnipod stelo g6 g7 g8
		libre minimed tandem mobi garzulys bysumlog ctx320 ctx310 vx-880 amg133 amg-133 sln360 tqj230 inaxaplin`) {
		hwDrugTerms[w] = true
	}
	for _, w := range strings.Fields(`insulin glp-1 glp1 gip amylin incretin sglt2 sglt-2 lpa cgm ketone pump
		biosensor sensor sirna antisense statin pcsk9 biosimilar generic`) {
		hwClassTerms[w] = true
	}
	for _, w := range strings.Fields(`a an the and or of in on for to with at by from as is are be been its it this
		that these their his her has have had was were will would could may can into about over under after before
		than more most less up down vs versus how why what who when where which new news first latest plus also
		announce announced announcement report reported says said update company companie corp inc ltd plc ag stock stocks
		studie therapie weekly daily monthly recap
		share shares price prices investor investors market data result study trial phase patient people adult
		drug drugs medicine medicines treatment therapy treat global week year today amid ahead us u.s s
		diabete diabetic obesity obese weight loss type heart health healthcare medical clinical pharma pharmaceutical
		biotech novo nordisk lilly eli sanofi dexcom insulet zealand novartis amgen ionis abbott medtronic vertex
		mannkind boehringer ingelheim roche astrazeneca merck crispr therapeutic`) {
		hwStopTerms[w] = true
	}
}

var hwTokenRe = regexp.MustCompile(`[a-z0-9]+(?:[.\-][a-z0-9]+)*`)
var hwYearRe = regexp.MustCompile(`^(19|20)\d\d$`)

// hwTokens lowercases a title into its terms. Lp(a) becomes lpa, plurals
// lose their s, and single characters and bare years are dropped.
func hwTokens(s string) []string {
	s = strings.ToLower(hwClean(s))
	s = strings.NewReplacer("lp(a)", "lpa", "lp (a)", "lpa", "lipoprotein(a)", "lpa", "’", "'", "®", " ", "™", " ").Replace(s)
	var out []string
	seen := map[string]bool{}
	for _, t := range hwTokenRe.FindAllString(s, -1) {
		listed := hwStopTerms[t] || hwDrugTerms[t] || hwClassTerms[t] // "novartis" keeps its s
		if !listed && len(t) > 4 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
			t = t[:len(t)-1]
		}
		if len(t) < 2 || hwYearRe.MatchString(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// hwIsDrugTerm reports a named drug, brand or device, including the
// development codes ("ly3457263", "amg-133") the lexicon does not list.
var hwCodeRe = regexp.MustCompile(`^[a-z]{2,5}-?\d{3,}[a-z]?$`)

func hwIsDrugTerm(t string) bool {
	return hwDrugTerms[t] || (hwCodeRe.MatchString(t) && !strings.HasPrefix(t, "nct"))
}

// hwTitleMatch reports whether a candidate title names the same thing as a
// coverage headline, with the shared terms that decided it.
func hwTitleMatch(coverage, candidate string) (bool, []string) {
	cand := map[string]bool{}
	for _, t := range hwTokens(candidate) {
		cand[t] = true
	}
	var drugs, classes, distinct []string
	for _, t := range hwTokens(coverage) {
		if !cand[t] || hwStopTerms[t] {
			continue
		}
		switch {
		case hwIsDrugTerm(t):
			drugs = append(drugs, t)
		case hwClassTerms[t]:
			classes = append(classes, t)
		case len(t) >= 3 || strings.ContainsAny(t, "0123456789"):
			distinct = append(distinct, t)
		}
	}
	shared := append(append(append([]string{}, drugs...), classes...), distinct...)
	ok := (len(drugs) >= 1 && len(distinct) >= 1) || len(drugs) >= 2 ||
		(len(classes) >= 1 && len(distinct) >= 2)
	return ok, shared
}

// A primary may be dated up to fourteen days before its coverage and four
// after. News trails the release: Medical Dialogues reported the FDA's
// ketone monitor authorisation of 2026-08-25 on 2026-09-07, and Bol News
// Lilly's insulin approval of 2026-09-23 on 2026-10-04. Four days after
// allows for a release dated in another time zone or a late feed date.
const (
	hwWindowBefore = 14
	hwWindowAfter  = 4
)

// hwInWindow reports whether a primary dated prim can be the source of
// coverage dated cov, both YYYY-MM-DD.
func hwInWindow(cov, prim string) bool {
	tc, err1 := time.Parse("2006-01-02", cov)
	tp, err2 := time.Parse("2006-01-02", prim)
	if err1 != nil || err2 != nil {
		return false
	}
	return !tp.Before(tc.AddDate(0, 0, -hwWindowBefore)) && !tp.After(tc.AddDate(0, 0, hwWindowAfter))
}

// ---------------------------------------------------------------------
// The resolver.
// ---------------------------------------------------------------------

// hwCand is a possible primary source for a coverage row.
type hwCand struct {
	url, title, source, kind, date, via string
}

type hwCov struct {
	url, title, topic, date, status string
	tries                           int
	detail                          map[string]any
}

type hwResolution struct {
	primary   hwCand
	matched   []string
	publisher string
	note      string
	self      bool // the publisher page is itself the primary
}

var hwPageClient = &http.Client{Timeout: 20 * time.Second}

// fetchPage gets a page for the resolver and returns its body and the URL
// it ended on after redirects. Every call counts against the run's budget.
func (r *hwRun) fetchPage(u string) (string, string, error) {
	r.fetches++
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", twoaiHealthPageUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	resp, err := hwPageClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	return string(b), resp.Request.URL.String(), err
}

// candidate reads a primary link's title. A DOI is read from Crossref and
// a trial from the ClinicalTrials.gov API, because the journal pages and
// the study page often refuse or need a script to show a title.
func (r *hwRun) candidate(u string, d hwDomain) (hwCand, error) {
	c := hwCand{url: u, source: d.owner, kind: hwRowKind(d), via: "publisher link"}
	switch d.domain {
	case "doi.org":
		doi := strings.TrimPrefix(u, "https://doi.org/")
		body, _, err := r.fetchPage("https://api.crossref.org/works/" + doi + "?mailto=" + twoaiHealthNCBIEmail)
		if err != nil {
			return c, err
		}
		var res struct {
			Message struct {
				Title     []string `json:"title"`
				Container []string `json:"container-title"`
				Published struct {
					Parts [][]int `json:"date-parts"`
				} `json:"published"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			return c, err
		}
		if len(res.Message.Title) > 0 {
			c.title = hwClean(res.Message.Title[0])
		}
		if len(res.Message.Container) > 0 {
			c.source = hwClean(res.Message.Container[0])
		}
		if p := res.Message.Published.Parts; len(p) > 0 && len(p[0]) == 3 {
			c.date = fmt.Sprintf("%04d-%02d-%02d", p[0][0], p[0][1], p[0][2])
		}
		return c, nil
	case "clinicaltrials.gov":
		nct := hwNCTRe.FindString(u)
		v := url.Values{}
		v.Set("filter.ids", nct)
		v.Set("fields", twoaiHealthTrialFields)
		body, _, err := r.fetchPage("https://clinicaltrials.gov/api/v2/studies?" + v.Encode())
		if err != nil {
			return c, err
		}
		trials, _, err := hwParseTrials([]byte(body))
		if err != nil || len(trials) == 0 {
			return c, fmt.Errorf("no study %s", nct)
		}
		t := trials[0]
		c.title, c.date = t.Title, t.Updated
		if t.Acronym != "" && !strings.Contains(c.title, t.Acronym) {
			c.title += " (" + t.Acronym + ")"
		}
		return c, nil
	}
	page, final, err := r.fetchPage(u)
	if err != nil {
		return c, err
	}
	// A redirect can leave the primary domain (a wire tracker going to a
	// home page); the row is filed under where the page really is.
	if pu, pd, ok := hwPrimaryURL(final); ok {
		c.url, c.source, c.kind = pu, pd.owner, hwRowKind(pd)
	}
	c.title, c.date = hwPageTitle(page), hwPageDate(page)
	if d.kind == "wire" {
		c.source = hwOwnerIn(c.title, d.owner)
	}
	return c, nil
}

// hwOwnerIn names the company a wire release is from: of the companies the
// watch knows, the one named first in its title, and the wire otherwise.
func hwOwnerIn(title, fallback string) string {
	lt := strings.ToLower(title)
	best, at := fallback, -1
	for _, d := range twoaiHealthPrimaryDomains {
		if d.kind != "company" || d.owner == "SEC EDGAR" {
			continue
		}
		i := strings.Index(lt, strings.ToLower(strings.SplitN(d.owner, " (", 2)[0]))
		if i >= 0 && (at < 0 || i < at) {
			best, at = d.owner, i
		}
	}
	return best
}

// matchKnown looks for the primary among the items the newsroom, society
// and regulator feeds carried this run and the rows already stored, dated
// within the window around the coverage (hwInWindow).
func (r *hwRun) matchKnown(c hwCov) (hwCand, []string) {
	pool := append([]hwCand{}, r.newsroom...)
	rows, err := r.db.Query(`SELECT url, title, source, kind, COALESCE(item_date::text,'') FROM twoai_health_watch
		WHERE kind <> 'coverage' AND item_date BETWEEN $1::date - $2::int AND $1::date + $3::int`,
		c.date, hwWindowBefore, hwWindowAfter)
	if err == nil {
		for rows.Next() {
			var p hwCand
			if rows.Scan(&p.url, &p.title, &p.source, &p.kind, &p.date) == nil {
				p.via = "stored row"
				pool = append(pool, p)
			}
		}
		rows.Close()
	}
	for _, p := range pool {
		if !hwInWindow(c.date, p.date) {
			continue
		}
		if ok, m := hwTitleMatch(c.title, p.title); ok {
			return p, m
		}
	}
	return hwCand{}, nil
}

// resolveOne looks for one coverage row's primary. It returns false when
// the deadline or the fetch budget cut the try short, which then does not
// count as a try.
func (r *hwRun) resolveOne(c hwCov) (hwResolution, bool) {
	var res hwResolution
	if p, m := r.matchKnown(c); p.url != "" {
		res.primary, res.matched = p, m
		return res, true
	}
	pub := c.url
	if p := hwStr(c.detail["resolve_publisher"]); p != "" {
		pub = p
	}
	if isGoogleNewsURL(pub) {
		r.fetches += 2
		pub = resolveGoogleNews(pub)
		if isGoogleNewsURL(pub) {
			res.note = "the Google News link did not resolve to a publisher"
			return res, true
		}
	}
	res.publisher = pub
	if r.late("coverage resolution") || r.fetches >= twoaiHealthResolveFetches {
		return res, false
	}
	page, final, err := r.fetchPage(pub)
	if err != nil {
		res.note = "publisher page: " + err.Error()
		return res, true
	}
	// The story may sit on a primary domain already, a wire release or a
	// company's own newsroom, and is then its own primary.
	if pu, d, ok := hwPrimaryURL(final); ok {
		t := hwPageTitle(page)
		if t == "" {
			t = c.title
		}
		src := d.owner
		if d.kind == "wire" {
			src = hwOwnerIn(t, d.owner)
		}
		res.primary = hwCand{url: pu, title: t, source: src, kind: hwRowKind(d), date: hwPageDate(page), via: "publisher page is primary"}
		res.matched, res.self = []string{"publisher domain " + d.domain}, true
		return res, true
	}
	links := hwPrimaryLinks(page, final, twoaiHealthResolveLinks)
	if len(links) == 0 {
		res.note = "no primary-source links on the publisher page"
		return res, true
	}
	var tried []string
	for _, l := range links {
		if r.late("coverage resolution") || r.fetches >= twoaiHealthResolveFetches {
			return res, false
		}
		_, d, _ := hwPrimaryURL(l)
		cand, err := r.candidate(l, d)
		if err != nil {
			tried = append(tried, l+" ("+err.Error()+")")
			continue
		}
		if ok, m := hwTitleMatch(c.title, cand.title); ok && cand.title != "" {
			res.primary, res.matched = cand, m
			return res, true
		}
		tried = append(tried, l+" (title did not match)")
	}
	res.note = "no linked page matched the headline: " + strings.Join(tried, "; ")
	return res, true
}

// record writes one try: the primary as its own row and the coverage row
// to 'resolved', or the try counted and, on the last, 'needs primary'.
func (r *hwRun) record(c hwCov, res hwResolution) {
	now := time.Now().UTC().Format(time.RFC3339)
	tries := c.tries + 1
	patch := map[string]any{"resolve_tries": tries, "resolve_last": now}
	if res.publisher != "" {
		patch["resolve_publisher"] = res.publisher
	}
	status := c.status
	p := res.primary
	switch {
	case p.url != "" && p.url == c.url:
		// The coverage row's own URL is the primary: refile it in place.
		patch["resolved_from"] = "coverage row refiled, its page is the primary"
		patch["resolve_matched"] = strings.Join(res.matched, ", ")
		patch["resolve_via"] = p.via
		pj, _ := json.Marshal(patch)
		if _, err := r.db.Exec(`UPDATE twoai_health_watch SET kind=$2, source=$3, status='new', detail = detail || $4::jsonb
			WHERE url=$1 AND status IN ('new','needs primary')`, c.url, p.kind, p.source, string(pj)); err != nil {
			r.notice("refile %s: %v", c.url, err)
			return
		}
		r.covResolved++
		return
	case p.url != "":
		it := hwItem{url: p.url, source: p.source, title: p.title, date: p.date, kind: p.kind, topic: c.topic,
			detail: map[string]any{"resolved_from": c.url, "resolved_matched": strings.Join(res.matched, ", "), "resolved_via": p.via}}
		if !r.save(it) {
			// Already stored: link it, and leave its status to the content
			// project, which may have read it already.
			r.db.Exec(`UPDATE twoai_health_watch SET detail = detail || jsonb_build_object('resolved_from', $2::text)
				WHERE url=$1 AND NOT detail ? 'resolved_from'`, p.url, c.url)
		}
		status = "resolved"
		patch["primary_url"] = p.url
		patch["resolved_at"] = now
		patch["resolve_matched"] = strings.Join(res.matched, ", ")
		patch["resolve_via"] = p.via
		r.covResolved++
	default:
		patch["resolve_note"] = res.note
		if tries >= twoaiHealthResolveTries {
			status = "needs primary"
			patch["resolve_gave_up"] = now
			r.covGaveUp++
		}
	}
	pj, _ := json.Marshal(patch)
	if _, err := r.db.Exec(`UPDATE twoai_health_watch SET status=$2, detail = detail || $3::jsonb
		WHERE url=$1 AND status IN ('new','needs primary')`, c.url, status, string(pj)); err != nil {
		r.notice("resolution of %s: %v", c.url, err)
	}
}

// resolveCoverage tries the coverage rows still without a primary, newest
// first and today's before the backlog, within the per-run bounds.
func (r *hwRun) resolveCoverage() {
	if r.late("coverage resolution") {
		return
	}
	rows, err := r.db.Query(`SELECT url, title, topic, COALESCE(item_date, found_on)::text, status, detail
		FROM twoai_health_watch
		WHERE kind='coverage' AND status IN ('new','needs primary')
		AND COALESCE((detail->>'resolve_tries')::int, 0) < $1
		AND (detail->>'resolve_last' IS NULL OR (detail->>'resolve_last')::timestamptz < $2)
		ORDER BY status='new' DESC, found_at DESC LIMIT $3`,
		twoaiHealthResolveTries, time.Now().Add(-twoaiHealthResolveGap), twoaiHealthResolveRows)
	if err != nil {
		r.notice("coverage resolution query: %v", err)
		return
	}
	var covs []hwCov
	for rows.Next() {
		var c hwCov
		var dj []byte
		if rows.Scan(&c.url, &c.title, &c.topic, &c.date, &c.status, &dj) != nil {
			continue
		}
		json.Unmarshal(dj, &c.detail)
		if c.detail == nil {
			c.detail = map[string]any{}
		}
		if n, ok := c.detail["resolve_tries"].(float64); ok {
			c.tries = int(n)
		}
		covs = append(covs, c)
	}
	rows.Close()
	for _, c := range covs {
		if r.late("coverage resolution") || r.fetches >= twoaiHealthResolveFetches {
			return
		}
		res, complete := r.resolveOne(c)
		if !complete {
			return
		}
		r.record(c, res)
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

// hwBridgeBody writes the bridge message. It stays near fifty lines
// whatever the day looks like: the counts are one line each and the two
// item lists are cut to fit. resolved is the number of coverage rows
// resolved to a primary since the last row, and unresolved lists those
// given up after their last try.
func hwBridgeBody(label, today, since string, n int, hubs, sources, verify []hwCount, rows []hwRow, resolved int, unresolved []hwRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s: %d new items since %s.\n", label, today, n, since)
	b.WriteString("All items are primary sources (PubMed, ClinicalTrials.gov, FDA, EMA, journal feeds, company and society newsrooms) except those marked coverage, which are news reports found through Google News and need a primary source before anything is published. Corrections, letters, labelling supplements, generic approvals and issue furniture are stored as skipped and not counted.\n")
	if resolved > 0 {
		fmt.Fprintf(&b, "Coverage resolved to its primary since then: %d (the primary is its own row with detail->>'resolved_from', the coverage row is status resolved).\n", resolved)
	}
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
	line := func(r hwRow) {
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
	if len(rows) > 0 {
		b.WriteString("Newest:\n")
	}
	for i, r := range rows {
		if i == 32 {
			break
		}
		line(r)
	}
	if len(rows) > 32 {
		fmt.Fprintf(&b, "... and %d more.\n", n-32)
	}
	if len(unresolved) > 0 {
		fmt.Fprintf(&b, "No primary found after %d tries, left at status needs primary (detail->>'resolve_note' says why):\n", twoaiHealthResolveTries)
		for i, r := range unresolved {
			if i == 6 {
				fmt.Fprintf(&b, "... and %d more.\n", len(unresolved)-6)
				break
			}
			line(r)
		}
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
	if err := r.db.QueryRow(`SELECT count(*) `+scope, topic, cutoff).Scan(&n); err != nil {
		return
	}
	var resolved int
	r.db.QueryRow(`SELECT count(*) FROM twoai_health_watch WHERE topic=$1 AND kind='coverage' AND status='resolved'
		AND (detail->>'resolved_at')::timestamptz > $2`, topic, cutoff).Scan(&resolved)
	var unresolved []hwRow
	if rows, err := r.db.Query(`SELECT kind, title, source, COALESCE(item_date::text,''), url, COALESCE(detail->>'verify','')
		FROM twoai_health_watch WHERE topic=$1 AND kind='coverage' AND status='needs primary'
		AND (detail->>'resolve_gave_up')::timestamptz > $2
		ORDER BY item_date DESC NULLS LAST, found_at DESC LIMIT 50`, topic, cutoff); err == nil {
		for rows.Next() {
			var x hwRow
			if rows.Scan(&x.kind, &x.title, &x.source, &x.date, &x.url, &x.verify) == nil {
				unresolved = append(unresolved, x)
			}
		}
		rows.Close()
	}
	if n == 0 && len(unresolved) == 0 {
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
	if len(unresolved) > 0 {
		subject += fmt.Sprintf(", %d coverage without a primary", len(unresolved))
	}
	body := hwBridgeBody(label, time.Now().UTC().Format("2006-01-02"), since, n, hubs, sources, verify, list, resolved, unresolved)
	if _, err := r.db.Exec(`INSERT INTO project_bridge (from_project, to_project, topic, body) VALUES ('srj','theworldofai',$1,$2)`, subject, body); err != nil {
		r.notice("bridge row for %s: %v", topic, err)
		return
	}
	r.db.Exec(`INSERT INTO twoai_health_watch_bridge (topic, items, bridge_topic) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, topic, n, subject)
	fmt.Printf("twoai_health_watch: bridge row sent, %s\n", subject)
}
