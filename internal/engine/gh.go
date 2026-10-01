package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Every GitHub call goes through ghRun, which runs the gh CLI (keeping gh's
// own auth) and classifies failures once:
//
//	class  ok | auth | sso | ratelimit | forbidden | notfound | network | error
//	http   HTTP status from gh's "(HTTP NNN)" suffix, or ""
//	msg    first line of gh's stderr, without the "gh: " prefix
//
// Results come back as Go values; the global/per-target error state the
// bash script kept in $PID_DIR files lives in ghStatus.

// ghExecFunc runs gh with args and returns stdout, stderr and the exit code
// (127 when it could not be started).
type ghExecFunc func(ctx context.Context, args []string) (stdout, stderr string, rc int)

const ghCallTimeout = 2 * time.Minute

func execGH(bin string) ghExecFunc {
	return func(ctx context.Context, args []string) (string, string, int) {
		var o, e bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		rc := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				rc = ee.ExitCode()
				if rc <= 0 {
					rc = 1
				}
			} else {
				rc = 127
				e.WriteString("gh: " + err.Error())
			}
		}
		return o.String(), e.String(), rc
	}
}

// ghResult is the classification of one gh call.
type ghResult struct {
	Class, HTTP, Msg string
}

func (r ghResult) ok() bool { return r.Class == "ok" }

type targetErr struct {
	class, msg string
	since      int64
}

// ghStatus is the in-memory replacement for the gh.* / target-*.err files.
type ghStatus struct {
	mu sync.Mutex

	// gh_auth_probe results
	probeState string // "" until the first probe
	probeMsg   string
	user, host string
	scopes     string // comma-separated, no spaces
	checked    int64

	// sticky global failure (auth|sso|ratelimit|network|error), cleared by
	// the next successful call
	errClass, errHTTP, errMsg string
	errAt                     int64

	rateKnown          bool
	rateRem, rateReset int64

	pollTS int64 // last successful runner listing

	targetErrs map[string]targetErr // by targetKey
}

var (
	reHTTP   = regexp.MustCompile(`\(HTTP ([0-9]{3})\)`)
	reSSOURL = regexp.MustCompile(`(?i)^x-github-sso:.*url=([^ ;]*)`)
	reLogin  = regexp.MustCompile(`"login": *"([^"]*)"`)

	reClsAuth = regexp.MustCompile(`not logged in|gh auth login|bad credentials|token.*expired|requires authentication`)
	reClsRate = regexp.MustCompile(`rate limit`)
	reClsSAML = regexp.MustCompile(`saml`)
	reClsNet  = regexp.MustCompile(`dial tcp|no such host|connection refused|timeout|timed out|tls|unreachable|network|error connecting`)
)

// ghClassify is the pure classifier (gh_classify).
func ghClassify(rc int, http, msg string) string {
	msg = strings.ToLower(msg)
	if rc == 0 {
		return "ok"
	}
	if rc == 4 || http == "401" || reClsAuth.MatchString(msg) {
		return "auth"
	}
	if http == "429" || (http == "403" && reClsRate.MatchString(msg)) {
		return "ratelimit"
	}
	if http == "403" && reClsSAML.MatchString(msg) {
		return "sso"
	}
	switch http {
	case "403":
		return "forbidden"
	case "404":
		return "notfound"
	}
	if http == "" && reClsNet.MatchString(msg) {
		return "network"
	}
	return "error"
}

// ghErrorNote is one line of human text per class (gh_error_note).
func ghErrorNote(class, msg string) string {
	var note string
	switch class {
	case "ok":
		note = "ok"
	case "auth":
		note = "gh is not logged in or its token was rejected - run: gh auth login"
	case "sso":
		note = "the organization enforces SAML SSO - authorize the gh token for it"
	case "ratelimit":
		note = "GitHub API rate limit reached - retrying after it resets"
	case "forbidden":
		note = "GitHub refused access (403) - the token lacks admin rights or scopes"
	case "notfound":
		note = "not found or no read access (404) - renamed, deleted, or access lost?"
	case "network":
		note = "cannot reach GitHub (network)"
	case "scope":
		note = ""
	default:
		note = "gh failed"
	}
	if msg != "" {
		if note != "" {
			note = note + " (" + msg + ")"
		} else {
			note = msg
		}
	}
	if note == "" {
		note = class
	}
	return note
}

// firstLine is the first non-blank line of s without a "gh: " prefix,
// at most 200 characters.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		line = strings.TrimPrefix(line, "gh: ")
		if r := []rune(line); len(r) > 200 {
			line = string(r[:200])
		}
		return line
	}
	return ""
}

func (e *Engine) ghCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.opCtx, timeout)
}

// ghRun runs gh ARGS, passes stdout back and classifies stderr (_gh_run).
func (e *Engine) ghRun(args ...string) (string, ghResult) {
	return e.ghRunTimeout(ghCallTimeout, args...)
}

func (e *Engine) ghRunTimeout(timeout time.Duration, args ...string) (string, ghResult) {
	ctx, cancel := e.ghCtx(timeout)
	defer cancel()
	out, errOut, rc := e.ghExec(ctx, args)
	http := ""
	if m := reHTTP.FindStringSubmatch(errOut); m != nil {
		http = m[1]
	}
	msg := firstLine(errOut)
	class := ghClassify(rc, http, msg)
	// SAML SSO: the authorize URL is only in a response header
	if class == "sso" && len(args) > 0 && args[0] == "api" && (len(args) < 2 || args[1] != "-i") {
		hout, _, _ := e.ghExec(ctx, append([]string{"api", "-i"}, args[1:]...))
		for _, line := range strings.Split(strings.ReplaceAll(hout, "\r", ""), "\n") {
			if m := reSSOURL.FindStringSubmatch(line); m != nil {
				msg += " - authorize: " + m[1]
				break
			}
		}
	}
	r := ghResult{Class: class, HTTP: http, Msg: msg}
	e.ghSetLast(r)
	return out, r
}

// ghSetLast records a call result: success clears the sticky global error,
// a global failure class sets it.
func (e *Engine) ghSetLast(r ghResult) {
	g := &e.ghs
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.Class {
	case "ok":
		g.errClass, g.errHTTP, g.errMsg, g.errAt = "", "", "", 0
	case "auth", "sso", "ratelimit", "network", "error":
		g.errClass, g.errHTTP, g.errMsg, g.errAt = r.Class, r.HTTP, r.Msg, e.now().Unix()
	}
}

// ghAPI is `gh api ARGS`.
func (e *Engine) ghAPI(args ...string) (string, ghResult) {
	return e.ghRun(append([]string{"api"}, args...)...)
}

// ghAPIHdr is `gh api -i ARGS`: response header lines and body split apart.
func (e *Engine) ghAPIHdr(args ...string) (headers []string, body string, r ghResult) {
	out, r := e.ghRun(append([]string{"api", "-i"}, args...)...)
	out = strings.ReplaceAll(out, "\r", "")
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if line == "" {
			return headers, strings.Join(lines[i+1:], "\n"), r
		}
		headers = append(headers, line)
	}
	return headers, "", r
}

// ghHeader finds one header (case-insensitive name) in header lines.
func ghHeader(headers []string, name string) (string, bool) {
	for _, h := range headers {
		k, v, found := strings.Cut(h, ":")
		if !found {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimLeft(v, " \t"), true
		}
	}
	return "", false
}

// ghState is the current gh state for display: a sticky global error wins
// over the probe result.
func (e *Engine) ghState() string {
	g := &e.ghs
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stateLocked()
}

func (g *ghStatus) stateLocked() string {
	if g.errClass != "" {
		return g.errClass
	}
	if g.probeState != "" {
		return g.probeState
	}
	return "unknown"
}

// ---- per-target errors ------------------------------------------------------

// targetSetError records an error; "since" is kept while the class is
// unchanged.
func (e *Engine) targetSetError(u, class, msg string) {
	msg = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(msg)
	g := &e.ghs
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.targetErrs == nil {
		g.targetErrs = map[string]targetErr{}
	}
	k := targetKey(u)
	since := e.now().Unix()
	if old, ok := g.targetErrs[k]; ok && old.class == class {
		since = old.since
	}
	g.targetErrs[k] = targetErr{class: class, msg: msg, since: since}
}

// targetClearError removes a target's error; with class != "", only an
// error of that class.
func (e *Engine) targetClearError(u, class string) {
	g := &e.ghs
	g.mu.Lock()
	defer g.mu.Unlock()
	k := targetKey(u)
	if old, ok := g.targetErrs[k]; ok && (class == "" || old.class == class) {
		delete(g.targetErrs, k)
	}
}

// targetClearErrorExcept removes a target's error unless it is of class.
func (e *Engine) targetClearErrorExcept(u, class string) {
	g := &e.ghs
	g.mu.Lock()
	defer g.mu.Unlock()
	k := targetKey(u)
	if old, ok := g.targetErrs[k]; ok && old.class != class {
		delete(g.targetErrs, k)
	}
}

func (e *Engine) targetError(u string) (targetErr, bool) {
	g := &e.ghs
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.targetErrs[targetKey(u)]
	return t, ok
}

// targetErrorNote is the human text for a target's stored error.
func (e *Engine) targetErrorNote(u string) string {
	t, ok := e.targetError(u)
	if !ok {
		return ""
	}
	return ghErrorNote(t.class, t.msg)
}

// targetAccessible checks the gh token can see the repo/org; on failure the
// target gets an error record.
func (e *Engine) targetAccessible(u string) ghResult {
	p := strings.TrimPrefix(u, ghPrefix)
	var r ghResult
	if targetType(u) == "repo" {
		_, r = e.ghAPI("repos/" + p)
	} else {
		_, r = e.ghAPI("orgs/" + p)
	}
	if !r.ok() {
		e.targetSetError(u, r.Class, r.Msg)
	}
	return r
}

// ---- registration tokens -----------------------------------------------------

// runnerToken mints a short-lived registration-token or remove-token.
func (e *Engine) runnerToken(kind, u string) (string, ghResult) {
	out, r := e.ghAPI("-X", "POST", targetAPIEndpoint(u)+"/"+kind, "--jq", ".token")
	tok := strings.TrimSpace(out)
	if r.ok() && tok == "" {
		r = ghResult{Class: "error", Msg: "GitHub returned no " + kind}
		e.ghSetLast(r)
	}
	if !r.ok() {
		// Keep the probe's clearer "scope" diagnosis over a bare 403
		old, _ := e.targetError(u)
		if old.class != "scope" || r.Class != "forbidden" {
			e.targetSetError(u, r.Class, r.Msg)
		}
		return "", r
	}
	e.targetClearError(u, "scope")
	return tok, r
}

func (e *Engine) registrationToken(u string) (string, ghResult) {
	return e.runnerToken("registration-token", u)
}

func (e *Engine) removeToken(u string) (string, ghResult) {
	return e.runnerToken("remove-token", u)
}

// ---- auth probe ---------------------------------------------------------------

// ghAuthProbe finds out who is logged in, with which scopes, and whether the
// token can register runners for each known target. Classic tokens list
// their scopes in X-OAuth-Scopes (org targets need admin:org, repos repo);
// for fine-grained/app tokens a registration token is minted per target
// instead. Returns false when the user call fails.
func (e *Engine) ghAuthProbe() bool {
	now := e.now().Unix()
	host := e.getenv("GH_HOST")
	if host == "" {
		host = "github.com"
	}
	g := &e.ghs
	g.mu.Lock()
	g.host, g.checked = host, now
	g.mu.Unlock()

	headers, body, r := e.ghAPIHdr("user")
	if !r.ok() {
		g.mu.Lock()
		g.probeState, g.probeMsg = r.Class, r.Msg
		g.mu.Unlock()
		return false
	}
	user := ""
	if m := reLogin.FindStringSubmatch(body); m != nil {
		user = m[1]
	}
	scopes, hasHdr := ghHeader(headers, "X-OAuth-Scopes")
	scopes = strings.NewReplacer(" ", "", "\t", "").Replace(scopes)
	g.mu.Lock()
	g.user, g.scopes, g.probeState, g.probeMsg = user, scopes, "ok", ""
	g.mu.Unlock()

	for _, t := range e.knownTargets() {
		if hasHdr {
			need := "repo"
			if targetType(t) == "org" {
				need = "admin:org"
			}
			if strings.Contains(","+scopes+",", ","+need+",") {
				e.targetClearError(t, "scope")
			} else {
				e.targetSetError(t, "scope", fmt.Sprintf("token lacks the %s scope - run: gh auth refresh -h %s -s %s", need, host, need))
			}
		} else if _, tr := e.registrationToken(t); tr.ok() {
			e.targetClearError(t, "scope")
		} else if tr.Class == "forbidden" {
			e.targetSetError(t, "scope", "token cannot register runners here (403) - it needs admin rights (fine-grained: 'Administration' for a repo, 'Self-hosted runners' for an org)")
		}
	}
	return true
}

// ---- rate limit ----------------------------------------------------------------

// ghRateOK reports whether a poll needing about need requests may run.
// /rate_limit is free. While the limit is exhausted the ratelimit state is
// kept until the reset time without calling gh.
func (e *Engine) ghRateOK(need int) bool {
	now := e.now().Unix()
	g := &e.ghs
	g.mu.Lock()
	if g.stateLocked() == "ratelimit" && g.rateKnown && now < g.rateReset {
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()
	out, r := e.ghAPI("rate_limit", "--jq", `.resources.core | "\(.remaining)\t\(.reset)"`)
	if !r.ok() {
		// Rate-limited or offline: skip this poll. Anything else goes on to
		// the normal poll, which re-probes and records per-target errors.
		return r.Class != "ratelimit" && r.Class != "network"
	}
	remS, resetS, _ := strings.Cut(strings.TrimSpace(out), "\t")
	rem, err1 := strconv.ParseInt(remS, 10, 64)
	reset, err2 := strconv.ParseInt(resetS, 10, 64)
	if err1 != nil || err2 != nil || rem < 0 || reset < 0 {
		return true
	}
	g.mu.Lock()
	g.rateKnown, g.rateRem, g.rateReset = true, rem, reset
	g.mu.Unlock()
	if rem < int64(need+20) {
		at := time.Unix(reset, 0).Format("15:04")
		e.ghSetLast(ghResult{Class: "ratelimit", Msg: fmt.Sprintf("rate limit exhausted (%d left), resumes %s", rem, at)})
		e.dlog(fmt.Sprintf("GitHub rate limit low (%d left) - polling paused until %s (runners keep running)", rem, at))
		return false
	}
	return true
}

// runnerGoneFromGithub is true when GitHub answers and does not list name
// under u, or answers 404 for the target itself.
func (e *Engine) runnerGoneFromGithub(u, name string) bool {
	if u == "" {
		return false
	}
	out, r := e.ghAPI("--paginate", targetAPIEndpoint(u), "--jq", ".runners[].name")
	if !r.ok() {
		return r.Class == "notfound"
	}
	for _, n := range strings.Split(out, "\n") {
		if n == name {
			return false
		}
	}
	return true
}

// githubStatusBody is what GitHub lists for each target plus the local
// orphan list, as plain text (github_status_body).
func (e *Engine) githubStatusBody() (string, bool) {
	var b strings.Builder
	ok := true
	any := false
	for _, t := range e.knownTargets() {
		any = true
		fmt.Fprintf(&b, "\n%s\n", targetLabel(t))
		out, r := e.ghAPI("--paginate", targetAPIEndpoint(t), "--jq", `.runners[] | "  \(.name): \(.status)"`)
		if r.ok() {
			out = strings.TrimRight(out, "\n")
			if out != "" {
				b.WriteString(out + "\n")
			} else {
				b.WriteString("  no runners registered\n")
			}
		} else {
			fmt.Fprintf(&b, "  Could not fetch: %s\n", ghErrorNote(r.Class, r.Msg))
			ok = false
		}
	}
	if !any {
		b.WriteString("No projects configured\n")
	}
	if lines := nonEmpty(readLines(e.orphansPath())); len(lines) > 0 {
		b.WriteString("\nOrphaned registrations recorded locally (failed unregisters):\n")
		for _, l := range lines {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("Remove them in GitHub Settings → Actions → Runners, then delete .orphaned-registrations\n")
	}
	return strings.TrimLeft(b.String(), "\n"), ok
}

func nonEmpty(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
