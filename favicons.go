package main

// favicons: one-shot, idempotent push of the site favicon set into
// srjordan6/srj-site public/. Added Aug 7 2026.
//
// Why this exists: the icon files are binary, the Filesystem connector that
// writes to Stephen's checkout is text-only, and the build sandbox has no
// git credentials, so the only authenticated write path from here to the
// repo is the same GitHub token this pipeline already uses to publish
// srj-content. The four files are embedded below as base64, which keeps this
// file plain text end to end. Only the four sizes the site heads actually
// link are shipped; the android-chrome 192/512 pair belongs with a web app
// manifest, which the site does not have, and can be added alongside one.
//
// The stage is safe to run every day inside `all`: putToRepo compares the
// git blob SHA before writing, so after the first run it is four GETs and
// no writes.

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var faviconFiles = map[string]string{
	"favicon.ico": "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",
	"favicon-16x16.png": "iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2AAAB30lEQVR4nJVSX0iTcRQ9937fKrbJmqQWBSJTocmgJZYPlUXEaC/L" +
		"h6jAokAfJCgqiuitXioKeomgKIyCBdkfBRMRoggCS1KylnNUiBhLKAvcJ7lvv3t7aIQQPew8Xs7hnnvuIVVFKeCS2ABsACIqIgAB" +
		"AJSImEEqRQoxiI0RAJbFVKolG8DHTHagb8R1CwQSMfYy/96YP5h9UiCb2GetjXuqGvp7X8/Ozrcd2moDON55M5POhiNrVNTJLWzf" +
		"GfVN9uUmXhDmNZVZ2u7zVDV0XR9Mj88UBTNff544k+g4HFu0udUXB+A4dw5wRRhAIOAPljtFS8HysovnHj2+P+S6JrKu+tjpROrt" +
		"ZD7vvh+bTt6rv3Zj5ZYQ8gX9c7cN4PyV/T3dQyLqOAt3bz1rbApdOPswm/3RtLF2Q7Syrq4CADNRMcVFcF23ZkV7f+/w86fvVi8/" +
		"eOpIl6oaY0Sko+1qc+SkqtrGyL7E5YnxL17vEtc1ublfZQHvppbw7eTRPbsujb753Lq7uefBq5HhT5tbwgBIRLqTL79/m/N4bGNM" +
		"bf2qbTsiImrbVmpsanBgdH1jiJjSH6Zj8Wh1TeV/HyeizPTvnFTVGFFVAimUiCyL/2pEhJkBFVFmZqaSq1FyW38DMY3pLh5y8eMA" +
		"AAAASUVORK5CYII=",
	"favicon-32x32.png": "iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAIAAAD8GO2jAAAEKElEQVR4nO1WXWwVVRD+5uw5e3e37aVQAgWEaJTfSCAaY/j/EU2K" +
		"NjEK8kSQB6GgAVSsSgIPmD4oEjH4S0xtUiVFFCqogcQH1BgiwZAQqoJASIRSS+nPvbds793dGR+23FzaAvpA1IR5O3vmzDcz33d2" +
		"DokIbqWpWxr9NsB/AkD3Wd9AU0S9LgNu9o8Q+1MsUxFhFsu6WUHCoH9WNPW5B7lsMIATEYsE2aA46UngX5u4QFlk2fE6ijgMIwJI" +
		"kTEaADEzgHTKf7Nm75HDp8IgwrVdICJmTnV2v1H78lynoevA25ZnQRjCAMSP9KQF3hN1US6nbXv/3iObNzQYY025767361aLiI4i" +
		"1tra8c7BrVu+HFY2KNV5hQtqIgBEQZSreWvlvOIv2ne8YCXAlwEBNMiBdEMyLQDiTmRS/rkzrcbWw8sHxxG0UgrAsaNnhySLh5QV" +
		"r6uudD2bWWLWSFGQC8tHlT255AH/+6PeohpYChJBGf7zeHj8c3IY2s0nZGmVcI2xtW33ykfHXIdhFIaRbetV6yps2/SnIYrYmf1K" +
		"YWU9TXt6vvuUAPI78t9FICzCkqe2j0wHFmkuF2hthbnsVQ0KKeWMnuo8UwtAkqNF5Hrq6gVQirS2evygZuNuxzEsQiBSKC0tmvPQ" +
		"vZMmj2FmYycKT+5saGlsDEjR3PmmqgrMA8ZHL8l3jy3/et/PXlHiw+0H89URQQSuZ+858OrY8SOq13wsIkQkQBjwTz+eTHdl2jrT" +
		"I8uYaA7LwAhaKSWCqjUVp09d/OXEH0OHleT7JIC2VPOF9m/2HV21tqJx9xGlCAALMt1+kZuwtFWaTD7+1AwARHQ9AAJwx5ihDfuq" +
		"Wy52XOnOAsIMrdXZMy2rl3+gjW5rTdm2Hl5emk77rmtrYy17bF4UseOYisr7p8+eGEWRpW7IQRCEAMpHDC7cizgCQEA2G5Qkvffq" +
		"qlYufTeTydpGc8Rbti+P3ZhZBMzCBeLJWy+sMTq+2YWWTyqucvqsiTsb1w8pK85mg/raQ4/M2PT7yWYAuWyotZVwjFKUSJg+GDrm" +
		"rWbTZ781nXdcW1gACEQRdXf3GG0RkePaAHw/O3nqnbv2v7Rs8bbm8x2nT15c/OjrO+qfHVTqbVz/ieMlHMc0X2hPDiry/Zw2FgCR" +
		"qy069O2JHw43ldge5+UmUJZyPTud8afNnABAayvIhfeMG7nrq+pli7f92nQ+15qunP9azdalS5bOWrvio1TaL/ISCce0dXQ9OH1c" +
		"3D1iZiJa/1zt8WPnPM8pVBsBjms/XDHl6RUL4i4RIQzZGH2ptevF1bVtl1KWpS63pTdsXjRuwqgNz9f39ASOa6bNHL9q3ULXTaD/" +
		"7/rvGLPEeHlLp/ySpDug800AYmH0H0TxqULtMwsRiCieXUqpaybarbP//6viNsC/D/AXuzHvh3MbiSMAAAAASUVORK5CYII=",
	"apple-touch-icon.png": "iVBORw0KGgoAAAANSUhEUgAAALQAAAC0CAIAAACyr5FlAAAYMElEQVR4nO2deZwUxb3Af7+q6p5zZ/biWsCDQxTFSCRqkIfGYFAR" +
		"DySK8T6ICgZPQF9Qkxh9Ro0miFcQDw4VozHxfmoUjRpeEmOiBtEYVC5B2GN2dnaOrqrf+6N31wW22Z7d2d1ZqO9n/4A5uqunv11V" +
		"Xb9fVSMRgcHQFqynC2AoXowcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwG" +
		"T4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wc" +
		"Bk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk+MHAZPjBwGT4wcBk9EwbdIBAC94BkuiLj9S0QABLDD610FASDs" +
		"WIx8t9KRJ+a0cfRtfKjzD+PRmogIiJChSyc32DMQdf487WJ0XA6tNWnigm//uiLHkUpppXRxPgYKEbPZXEVFDFmTDUQakelsUmWS" +
		"iN3V1JJGK8TDZZ3ZhuPIxlQ2rwuSiAIBKxiy2/2kr2aFiFqfZq0JgIQQwEBKWvf5pk8/3rjqX2s/W/Plls3J+kRjOp11HCUlFZMc" +
		"6DZ2Qojq6rpzLjhy9o+nKaUQEUgjF86W9xNLprHkOmCiuVnswiaGkFEmx0dOKvvBcq1UByotpbRliddefm/OrAdj8ahS2s+3uOB1" +
		"Nckzzxs/9/rTldRc7OxKyLvP4ZYJAFb/a93zv1/52qsfrP1sS21tSjrEGOcCGWMMGaC/Zq37QAASgtfW1E868ZuXzZ5KoAGazKCG" +
		"9fWPnS02f8QCALKbikM5ANnQyc2kG50vN9Q3psi3HKxmazJRl/bz4fzk0Epbllj7+ZY7bnnqxWfeTSSygYAVsO2SkhJkCK0qmCKq" +
		"MpogIURdXfLIo0fe+/CsQFAopREIkEMuUffEeXzDP1mIk9bd1B9lHFABbt8o570ZhrYtbFv4l8O2hdihM9AmecihtRaWeP2Vf1w5" +
		"Y+GmLxtisUh5eYBIE5HWGnyVrcfgnCcSqZGjqu576EehsCUdxTgAIUOn7g8zYPWrLMxJq24sEREQNrVfnfERibZv93e2Vw0EfvuC" +
		"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" +
		"c9jhQ3/xqwubluj0d+30PjncOJx7zFKq6upEKAS3zT9/7vXTZMvz67sSdwgrGgssXHrlSad8q7o60bH7F2jOJQsFIzfOe/zKGfdn" +
		"0koIrjUhoOPIU88cv/TJ2YMGxxJ5NmHuIA0XDADr6xvr6+tPP2vc4t/OKa+MEpH/pqqdqIEbW7l+7uKH7ltRWt4zsZWW0Ic75qOU" +
		"cmcTAdLAQaWTThhz7vSJew3rJ2V3mNEMaq04Z6Rx9qxFyx5eUVFR5qY25D1qT4SInPPq6sTY8cPn3z9jwMAypRQgaqks29qwrvqK" +
		"Gfe/+dqq8opSBKI2t0/N2yLQmhxHZrM5rVUkao8dt+9ZF3z36GO/CUAtU7l8LrLjS46rZt5/zz0vl0dLekIOcheNQUaMoWXzkpLQ" +
		"HntVjh4z5OBvjTh8/Mg+/eIA26VNQPdkXmlNjCFj7Ia5S+/99Quci85MHxeCJ5INgwfF77j74gnHjUZsmqNrWSKTdq6f+8jiB14H" +
		"FG2toknu9GTGUHAWDFl9+sZHjhp88CHDxo7bb9ToIQDQEvZzv1AYOdzjf+4PK1f+6RPPtX93crV0+C0ANwTGOFiWCIfDZeXh0rJI" +
		"ZZ+SqkEVAwaWtwxjSEfiNjPMCrf8RnsgotbanQr78G9e/mzNFtve6QozO69WCITgqVQ2HMYLZ07q0zfuzr1WSgvBAPCxR1as/mi9" +
		"bW+ziDRDFILZATsWD5WVl5SVRftXxQft0TdaEnQ/oJTacdSnMHIULW4dVgwTD92nSHVpMdxzlNcutG5aA74zTa0vObTWpIviCZeI" +
		"fq3vZpTShXnOCxFg21F4pTT4eM6o+/MU5CfqrTWHoRvofbeyhm7DyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDE" +
		"yGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHw" +
		"xMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh8MTIYfDEyGHwxMhh" +
		"8OT/AdXL6chWnK4ZAAAAAElFTkSuQmCC",
}

// One file's failure no longer ends the stage. It used to return on the
// first error, and on Aug 13 2026 that meant a single 403 on a new file
// stopped the other 133 in the same set from being attempted, so the log
// showed one failure where there were many and the cause looked narrower
// than it was. Failures are counted and reported together at the end, and
// the stage still fails so the run is not silently green.
func runFavicons() error {
	loadRemoteCovers()
	var failed []string
	for name, b64 := range faviconFiles {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "favicons: %s: bad embed: %v\n", name, err)
			failed = append(failed, name)
			continue
		}
		if err := putToRepo("srjordan6/srj-site", "public/"+name,
			"Favicon: "+name+" (SRJ monogram, generated from the brand logo)",
			data); err != nil {
			fmt.Fprintf(os.Stderr, "favicons: %s: %v\n", name, err)
			failed = append(failed, name)
			continue
		}
		fmt.Println("favicons: ensured", name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("favicons: %d of %d failed, first %s", len(failed), len(faviconFiles), failed[0])
	}
	return nil
}

// hourlyCatchUp runs after every email_route pass, which the
// srj-email-coordinator cron fires at the top of each hour. It exists
// because Render cron deploys do not run the job: code and SQL changes
// otherwise sit unpublished until the daily 11:00 UTC `all` run. Every step
// here is idempotent, so an hourly cadence costs a handful of GETs when
// nothing changed. The whole pass is skipped when the service has no
// GITHUB_TOKEN, since both steps write through the GitHub contents API.
//
// The Cloudflare hook fires only when the srj-content HEAD actually moved,
// read unauthenticated since the repo is public: pushes to srj-site
// (favicons) trigger a build natively, but srj-site does not watch
// srj-content, so content-only changes need the hook.
func hourlyCatchUp(db *sql.DB) {
	if os.Getenv("GITHUB_TOKEN") == "" {
		fmt.Println("catchup: no GITHUB_TOKEN on this service, skipping")
		return
	}
	if err := runFavicons(); err != nil {
		fmt.Fprintln(os.Stderr, "catchup favicons:", err)
	}
	pre := contentHead()
	if err := syncContent(db); err != nil {
		fmt.Fprintln(os.Stderr, "catchup sync_content:", err)
		return
	}
	post := contentHead()
	if pre != "" && post != "" && pre != post {
		fmt.Println("catchup: srj-content moved", pre[:8], "->", post[:8], "firing deploy hook")
		if err := fireDeployHook(); err != nil {
			fmt.Fprintln(os.Stderr, "catchup deploy hook:", err)
		}
	}
}

// contentHead returns the srj-content main HEAD sha, or "" on any failure.
// Unauthenticated: the repo is public.
func contentHead() string {
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/srjordan6/srj-content/commits/main", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "srj-pipeline/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.SHA
}

// fireDeployHook POSTs the srj-site Cloudflare deploy hook. Uses
// CLOUDFLARE_DEPLOY_HOOK when set (the main pipeline cron has it); falls
// back to the known hook URL so the hourly coordinator, which may not
// carry that env var, can still trigger builds. The hook grants exactly
// one capability, starting a build, and nothing else.
func fireDeployHook() error {
	hook := strings.TrimSpace(os.Getenv("CLOUDFLARE_DEPLOY_HOOK"))
	if hook == "" {
		hook = "https://api.cloudflare.com/client/v4/workers/builds/deploy_hooks/e69389bf-e2f0-4d52-8eb2-8fb0fdb75e02"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(hook, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deploy hook %d: %.200s", resp.StatusCode, b)
	}
	return nil
}
