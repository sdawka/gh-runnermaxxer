package engine

import (
	"strings"
	"testing"
)

func TestGHClassify(t *testing.T) {
	cases := []struct {
		rc         int
		http, msg  string
		want, name string
	}{
		{0, "", "", "ok", "rc 0"},
		{4, "", "To get started with GitHub CLI, please run:  gh auth login", "auth", "rc 4"},
		{1, "401", "Bad credentials (HTTP 401)", "auth", "HTTP 401"},
		{1, "", "You are not logged into any GitHub hosts. Run gh auth login to authenticate.", "auth", "not logged in"},
		{1, "403", "API rate limit exceeded for user ID 1. (HTTP 403)", "ratelimit", "403 + rate limit"},
		{1, "403", "You have exceeded a secondary rate limit (HTTP 403)", "ratelimit", "secondary rate limit"},
		{1, "429", "Too Many Requests (HTTP 429)", "ratelimit", "429"},
		{1, "403", "Resource protected by organization SAML enforcement. (HTTP 403)", "sso", "SAML"},
		{1, "403", "Resource not accessible by integration (HTTP 403)", "forbidden", "plain 403"},
		{1, "404", "Not Found (HTTP 404)", "notfound", "404"},
		{1, "", "error connecting to api.github.com", "network", "error connecting"},
		{1, "", `Get "https://api.github.com/user": dial tcp: lookup api.github.com: no such host`, "network", "dial tcp"},
		{1, "", "unexpected end of JSON input", "error", "anything else"},
		{1, "500", "Server Error (HTTP 500)", "error", "500"},
	}
	for _, c := range cases {
		eq(t, c.want, ghClassify(c.rc, c.http, c.msg), c.name)
	}
}

func TestGHErrorNote(t *testing.T) {
	contains(t, ghErrorNote("auth", ""), "gh auth login", "auth note")
	n := ghErrorNote("notfound", "Not Found")
	contains(t, n, "404", "notfound note")
	if !strings.HasSuffix(n, "(Not Found)") {
		t.Errorf("notfound note should end with the message: %q", n)
	}
	eq(t, "token lacks admin:org", ghErrorNote("scope", "token lacks admin:org"), "scope note is the message")
}

func TestGHRunClassifiesAndKeepsState(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	te.gh.set(func([]string) (string, string, int) { return "body\n", "gh: Not Found (HTTP 404)\n", 1 })
	out, r := e.ghAPI("repos/o/r")
	eq(t, "body\n", out, "stdout passed through")
	eq(t, ghResult{"notfound", "404", "Not Found (HTTP 404)"}, r, "classified")
	eq(t, "unknown", e.ghState(), "target-specific class is not a global error")

	te.gh.set(func([]string) (string, string, int) { return "", "gh: dial tcp: i/o timeout", 1 })
	_, r = e.ghAPI("user")
	eq(t, ghResult{"network", "", "dial tcp: i/o timeout"}, r, "empty HTTP field")
	eq(t, "network", e.ghState(), "network failure is global")

	te.gh.set(func([]string) (string, string, int) { return "", "gh: Bad credentials (HTTP 401)", 1 })
	e.ghAPI("user")
	eq(t, "auth", e.ghState(), "sticky global error")
	te.gh.set(func([]string) (string, string, int) { return "{}", "", 0 })
	_, r = e.ghAPI("user")
	eq(t, "ok", r.Class, "success")
	eq(t, "unknown", e.ghState(), "success clears the global error; unknown before any probe")

	// a gh that can't be started
	e.ghExec = execGH("/nonexistent/gh-binary")
	_, r = e.ghAPI("user")
	eq(t, false, r.ok(), "missing binary fails")
}

func TestGHBinaryOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNNERMAXXER_GH", "/bin/echo")
	e, err := newEngine(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	out, r := e.ghAPI("x")
	eq(t, "api x\n", out, "RUNNERMAXXER_GH selects the binary")
	eq(t, "ok", r.Class, "ok")
	t.Setenv("RUNNERMAXXER_GH", "/nonexistent")
	e, _ = newEngine(Options{Dir: dir, GH: "/bin/echo"})
	out, _ = e.ghAPI("y")
	eq(t, "api y\n", out, "Options.GH wins")
}

func TestGHHeaders(t *testing.T) {
	te := newTestEnv(t, "")
	te.gh.set(func([]string) (string, string, int) {
		return "HTTP/2.0 200 OK\r\nContent-Type: application/json\r\nX-Oauth-Scopes: repo, admin:org\r\n\r\n{\"login\":\"sahil\"}\n", "", 0
	})
	hdr, body, _ := te.e.ghAPIHdr("user")
	eq(t, "{\"login\":\"sahil\"}\n", body, "body only")
	v, ok := ghHeader(hdr, "X-OAuth-Scopes")
	eq(t, "repo, admin:org", v, "case-insensitive, CR stripped")
	eq(t, true, ok, "present")
	v, _ = ghHeader(hdr, "content-type")
	eq(t, "application/json", v, "lower-case name")
	_, ok = ghHeader(hdr, "X-GitHub-SSO")
	eq(t, false, ok, "absent")
	v, ok = ghHeader([]string{"HTTP/2.0 200 OK", "X-OAuth-Scopes: "}, "X-OAuth-Scopes")
	eq(t, true, ok, "present but empty")
	eq(t, "", v, "empty value")
}

func TestGHSSORetry(t *testing.T) {
	te := newTestEnv(t, "")
	te.gh.set(func(args []string) (string, string, int) {
		out := ""
		if len(args) > 1 && args[1] == "-i" {
			out = "HTTP/2.0 403 Forbidden\r\nX-GitHub-SSO: required; url=https://github.com/orgs/o/sso?authorization_request=1\r\n\r\n{}\n"
		}
		return out, "gh: Resource protected by organization SAML enforcement. (HTTP 403)", 1
	})
	_, r := te.e.ghAPI("orgs/o")
	eq(t, "sso", r.Class, "SAML -> sso")
	if !strings.HasSuffix(r.Msg, "authorize: https://github.com/orgs/o/sso?authorization_request=1") {
		t.Errorf("sso message lacks the authorize URL: %q", r.Msg)
	}
}

func TestTargetErrors(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/o/r"
	e.targetSetError(u, "notfound", "Not Found")
	got, ok := e.targetError(u)
	eq(t, true, ok, "set")
	eq(t, "notfound/Not Found", got.class+"/"+got.msg, "class and msg")
	e.ghs.targetErrs["o+r"] = targetErr{class: "notfound", msg: "Not Found", since: 100}
	e.targetSetError(u, "notfound", "Not Found again")
	got, _ = e.targetError(u)
	eq(t, int64(100), got.since, "same class keeps since")
	e.targetSetError(u, "forbidden", "multi\tline\nmsg")
	got, _ = e.targetError(u)
	eq(t, "multi line msg", got.msg, "flattened")
	if got.since == 100 {
		t.Error("new class should reset since")
	}
	e.targetClearError(u, "scope")
	_, ok = e.targetError(u)
	eq(t, true, ok, "clear with another class keeps it")
	e.targetClearErrorExcept(u, "forbidden")
	_, ok = e.targetError(u)
	eq(t, true, ok, "clear_except of the same class keeps it")
	e.targetClearError(u, "")
	_, ok = e.targetError(u)
	eq(t, false, ok, "cleared")
}

func TestGHCallSites(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	u := "https://github.com/o/r"
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Not Found (HTTP 404)", 1 })
	r := e.targetAccessible(u)
	eq(t, "notfound", r.Class, "targetAccessible 404")
	got, _ := e.targetError(u)
	eq(t, "notfound", got.class, "records the target error")
	e.targetClearError(u, "")
	eq(t, true, e.runnerGoneFromGithub(u, "mac-1"), "404 for the target counts as gone")
	te.gh.set(func([]string) (string, string, int) { return "", "gh: Must have admin rights (HTTP 403)", 1 })
	eq(t, false, e.runnerGoneFromGithub(u, "mac-1"), "403 is not gone")
	te.gh.set(func([]string) (string, string, int) { return "mac-2\n", "", 0 })
	eq(t, true, e.runnerGoneFromGithub(u, "mac-1"), "listed without the name")
	eq(t, false, e.runnerGoneFromGithub(u, "mac-2"), "listed")

	te.gh.set(func([]string) (string, string, int) { return "", "gh: API rate limit exceeded (HTTP 403)", 1 })
	_, ok := e.targetQueueDepth(u, 1, 1)
	eq(t, false, ok, "queue depth fails on an API error")
	got, _ = e.targetError(u)
	eq(t, "ratelimit", got.class, "queue depth records the class")
}

func TestRegistrationTokens(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	repo, org := "https://github.com/o/r", "https://github.com/myorg"
	var last string
	te.gh.set(func(args []string) (string, string, int) {
		last = strings.Join(args, " ")
		return "tok123\n", "", 0
	})
	tok, _ := e.registrationToken(repo)
	eq(t, "tok123", tok, "token")
	eq(t, "api -X POST repos/o/r/actions/runners/registration-token --jq .token", last, "repo endpoint")
	e.registrationToken(org)
	eq(t, "api -X POST orgs/myorg/actions/runners/registration-token --jq .token", last, "org endpoint")
	e.removeToken(repo)
	eq(t, "api -X POST repos/o/r/actions/runners/remove-token --jq .token", last, "remove-token")

	te.gh.set(func([]string) (string, string, int) {
		return "", "gh: Must have admin rights to Repository. (HTTP 403)", 1
	})
	_, r := e.registrationToken(repo)
	eq(t, false, r.ok(), "403 fails")
	got, _ := e.targetError(repo)
	eq(t, "forbidden", got.class, "recorded as forbidden")
	e.targetSetError(repo, "scope", "token lacks the repo scope")
	e.registrationToken(repo)
	got, _ = e.targetError(repo)
	eq(t, "scope", got.class, "scope error survives a 403")
	te.gh.set(func([]string) (string, string, int) { return "tok123", "", 0 })
	e.registrationToken(repo)
	_, ok := e.targetError(repo)
	eq(t, false, ok, "success clears the scope error")
	te.gh.set(func([]string) (string, string, int) { return "", "", 0 })
	_, r = e.registrationToken(repo)
	eq(t, false, r.ok(), "empty token is a failure")
}

func TestAuthProbe(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	te.targets("myorg\no/r\n")
	org, repo := "https://github.com/myorg", "https://github.com/o/r"

	te.gh.set(func([]string) (string, string, int) {
		return "HTTP/2.0 200 OK\r\nX-Oauth-Scopes: repo, read:org\r\n\r\n{\"login\":\"sahil\",\"id\":1}\n", "", 0
	})
	eq(t, true, e.ghAuthProbe(), "probe ok")
	eq(t, "sahil", e.ghs.user, "user")
	eq(t, "repo,read:org", e.ghs.scopes, "scopes")
	eq(t, "ok", e.ghState(), "state")
	eq(t, "github.com", e.ghs.host, "host default")
	got, _ := e.targetError(org)
	eq(t, "scope", got.class, "org without admin:org")
	contains(t, got.msg, "admin:org", "names the scope")
	contains(t, got.msg, "gh auth refresh", "names the fix")
	_, ok := e.targetError(repo)
	eq(t, false, ok, "repo with repo scope is clear")
	eq(t, 0, te.gh.count("registration-token"), "classic token: no token minted")
	msg, _ := e.GHStatus()
	contains(t, msg, "✓ gh: sahil (repo, read:org)", "summary")
	contains(t, msg, "myorg (organization): token lacks", "scope problem")

	te.gh.set(func([]string) (string, string, int) {
		return "HTTP/2.0 200 OK\r\nX-OAuth-Scopes: repo, admin:org\r\n\r\n{\"login\":\"sahil\"}\n", "", 0
	})
	e.ghAuthProbe()
	_, ok = e.targetError(org)
	eq(t, false, ok, "scope error clears once granted")
	e.targetSetError(repo, "notfound", "Not Found")
	e.ghAuthProbe()
	got, _ = e.targetError(repo)
	eq(t, "notfound", got.class, "non-scope error left alone")
	e.targetClearError(repo, "")

	// fine-grained token
	te.gh.set(func(args []string) (string, string, int) {
		a := strings.Join(args, " ")
		switch {
		case strings.Contains(a, "-i user"):
			return "HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n{\"login\":\"sahil\"}\n", "", 0
		case strings.Contains(a, "orgs/myorg/actions/runners/registration-token"):
			return "", "gh: Resource not accessible by personal access token (HTTP 403)", 1
		case strings.Contains(a, "registration-token"):
			return "tok", "", 0
		}
		return "", "", 0
	})
	e.ghAuthProbe()
	eq(t, "", e.ghs.scopes, "no scope header")
	eq(t, 2, te.gh.count("registration-token"), "one token per target")
	got, _ = e.targetError(org)
	eq(t, "scope", got.class, "403 -> scope")
	_, ok = e.targetError(repo)
	eq(t, false, ok, "successful mint -> clear")
	msg, _ = e.GHStatus()
	contains(t, msg, "fine-grained", "summary says no scope list")

	te.gh.set(func([]string) (string, string, int) { return "", "gh: Bad credentials (HTTP 401)", 1 })
	eq(t, false, e.ghAuthProbe(), "401 fails the probe")
	eq(t, "auth", e.ghs.probeState, "probe state")
	eq(t, "Bad credentials (HTTP 401)", e.ghs.probeMsg, "probe message")
	eq(t, "auth", e.ghState(), "state auth")
	if _, err := e.GHStatus(); err == nil {
		t.Error("GHStatus should fail when gh is not logged in")
	}
}

func TestHealthReprobesWhileBroken(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	te.gh.set(func(args []string) (string, string, int) {
		a := strings.Join(args, " ")
		switch {
		case strings.Contains(a, "rate_limit"):
			return "5000\t1\n", "", 0
		case strings.Contains(a, "-i user"):
			return "HTTP/2.0 200 OK\r\n\r\n{\"login\":\"x\"}", "", 0
		}
		return "{}", "", 0
	})
	e.checkGithubHealth()
	eq(t, 1, te.gh.count("-i user"), "re-probes when state != ok")
	e.checkGithubHealth()
	eq(t, 1, te.gh.count("-i user"), "skips the probe when ok")
}
