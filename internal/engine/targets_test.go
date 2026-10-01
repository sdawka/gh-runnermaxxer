package engine

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeAndEntryToURL(t *testing.T) {
	const want = "https://github.com/owner/repo"
	cases := []struct{ fn, in, want string }{
		{"norm", "https://github.com/owner/repo/", want},
		{"norm", "https://github.com/owner/repo.git", want},
		{"norm", "https://github.com/owner/repo.git/", want},
		{"norm", "https://github.com/owner/repo", want},
		{"norm", "git@github.com:owner/repo.git", want},
		{"norm", "git@github.com:owner/repo", want},
		{"norm", "ssh://git@github.com/owner/repo.git", want},
		{"entry", "git@github.com:owner/repo.git", want},
		{"entry", "ssh://git@github.com/owner/repo", want},
		{"entry", "owner/repo", want},
		{"entry", "myorg", "https://github.com/myorg"},
		{"entry", "https://github.com/owner/repo", want},
		{"entry", "http://github.com/owner/repo", want},
		{"entry", "github.com/owner/repo", want},
		{"entry", "", ""},
		{"entry", "# a comment", ""},
		{"entry", "  owner/repo  ", want},
	}
	for _, c := range cases {
		got := ""
		if c.fn == "norm" {
			got = normalizeURL(c.in)
		} else {
			got = targetEntryToURL(c.in)
		}
		eq(t, c.want, got, c.fn+" "+c.in)
	}
}

func TestTargetURLHelpers(t *testing.T) {
	repo, org := "https://github.com/owner/repo", "https://github.com/myorg"
	eq(t, true, validRepoURL(repo), "repo url")
	eq(t, false, validRepoURL("https://github.com/owner"), "org is not a repo")
	eq(t, true, validOrgURL("https://github.com/owner"), "org url")
	eq(t, false, validOrgURL(repo), "repo is not an org")
	eq(t, "repo", targetType(repo), "type repo")
	eq(t, "org", targetType(org), "type org")
	eq(t, "owner/repo", targetLabel(repo), "label repo")
	eq(t, "myorg (organization)", targetLabel(org), "label org")
	eq(t, true, sameTarget("https://github.com/Owner/Repo", repo), "case-insensitive")
	eq(t, false, sameTarget(repo, "https://github.com/owner/other"), "different")
	eq(t, false, sameTarget("", repo), "empty first arg")
	eq(t, "repos/owner/repo/actions/runners", targetAPIEndpoint(repo), "endpoint repo")
	eq(t, "orgs/myorg/actions/runners", targetAPIEndpoint(org), "endpoint org")
	eq(t, "owner+repo", targetKey("https://github.com/Owner/Repo"), "key")
}

func TestSplitTargetLine(t *testing.T) {
	cases := []struct {
		line    string
		ok, has bool
		entry   string
		mn, mx  int
	}{
		{"o/r", true, false, "o/r", 0, 0},
		{"o/r min=1 max=5", true, true, "o/r", 1, 5},
		{"o/r max=3", true, true, "o/r", 0, 3},
		{"o/r min=2", true, true, "o/r", 2, 20},
		{"o/r min=1 max=5 # note", true, true, "o/r", 1, 5},
		{"o/r min=6 max=5", false, false, "o/r", 0, 0},
		{"o/r size=5", false, false, "o/r", 0, 0},
		{"o/r min=x", false, false, "o/r", 0, 0},
		{"# comment", true, false, "", 0, 0},
		{"", true, false, "", 0, 0},
	}
	for _, c := range cases {
		tl, ok := splitTargetLine(c.line, 20)
		eq(t, c.ok, ok, c.line+" ok")
		if !c.ok {
			continue
		}
		eq(t, c.entry, tl.entry, c.line+" entry")
		eq(t, c.has, tl.hasBounds, c.line+" bounds")
		if c.has {
			eq(t, c.mn, tl.min, c.line+" min")
			eq(t, c.mx, tl.max, c.line+" max")
		}
	}
}

func TestTargetsFile(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"20\"\n")
	f := te.e.targets
	repo, org := "https://github.com/owner/repo", "https://github.com/myorg"
	f.add(repo)
	eq(t, true, f.contains(repo), "finds a just-added target")
	eq(t, true, f.contains("https://github.com/OWNER/REPO"), "case-insensitive")
	eq(t, false, f.contains("https://github.com/other/repo"), "unlisted")
	f.add(repo)
	data, _ := os.ReadFile(f.path)
	eq(t, 1, strings.Count(string(data), "\nowner/repo\n"), "no duplicate line")
	f.add(org)
	eq(t, true, f.contains(org), "second target")

	te.targets("# a comment\n\nowner/repo\nowner/repo\nOWNER/REPO\nnot a valid target!!\nmyorg\n")
	eq(t, "https://github.com/owner/repo,https://github.com/owner/repo,https://github.com/OWNER/REPO,https://github.com/myorg",
		strings.Join(f.load(), ","), "load expands in order, drops invalid")
	eq(t, "https://github.com/owner/repo,https://github.com/myorg", strings.Join(te.e.knownTargets(), ","), "known targets dedupe")
	eq(t, "not a valid target!!", strings.Join(f.invalid(), ","), "invalid lines")

	f.remove(repo)
	eq(t, false, f.contains(repo), "removed")
	eq(t, true, f.contains(org), "unrelated survives")
	data, _ = os.ReadFile(f.path)
	contains(t, string(data), "# a comment\n", "comment survives")

	// missing trailing newline is fixed before appending
	te.targets("myorg")
	f.add(repo)
	data, _ = os.ReadFile(f.path)
	eq(t, "myorg\nowner/repo\n", string(data), "trailing newline fixed")

	// known targets: runners registered to unlisted targets follow
	te.runner(1, "https://github.com/x/unlisted")
	eq(t, "https://github.com/myorg,https://github.com/owner/repo,https://github.com/x/unlisted", strings.Join(te.e.knownTargets(), ","), "registered targets appended")
}

func TestTargetBounds(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"20\"\n")
	f := te.e.targets
	u := "https://github.com/o/r"
	te.targets("o/r # keep\n")
	_, _, ok := f.bounds(u)
	eq(t, false, ok, "no bounds")
	f.setBounds(u, true, 1, 4)
	mn, mx, ok := f.bounds(u)
	eq(t, true, ok, "bounds set")
	eq(t, 1, mn, "min")
	eq(t, 4, mx, "max")
	f.setBounds(u, false, 0, 0)
	_, _, ok = f.bounds(u)
	eq(t, false, ok, "bounds cleared")
	f.setBounds("https://github.com/new/one", true, 0, 2)
	eq(t, true, f.contains("https://github.com/new/one"), "unlisted target appended")

	msg, err := te.e.SetBounds("o/r", 2, 30)
	if err == nil {
		t.Errorf("max over MAX_RUNNERS accepted: %s", msg)
	}
	msg, err = te.e.SetBounds("o/r", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, msg, "o/r: autoscale between 2 and 3", "message")
	contains(t, msg, "AUTOSCALE=0", "autoscale-off note")
	msg, _ = te.e.SetBounds("o/r", 0, 0)
	eq(t, "o/r: bounds cleared (fixed count)", msg, "clear message")
}

func TestAddRemoveTarget(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"20\"\n")
	te.gh.set(func(args []string) (string, string, int) {
		if strings.Contains(strings.Join(args, " "), "repos/o/gone") {
			return "", "gh: Not Found (HTTP 404)", 1
		}
		return "{}", "", 0
	})
	if _, err := te.e.AddTarget("not valid!!"); err == nil {
		t.Error("invalid target accepted")
	}
	_, err := te.e.AddTarget("o/gone")
	if err == nil {
		t.Fatal("inaccessible target accepted")
	}
	contains(t, err.Error(), "can't use o/gone", "error")
	msg, err := te.e.AddTarget("https://github.com/o/r.git")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "added o/r", msg, "added")
	msg, _ = te.e.AddTarget("o/r")
	eq(t, "o/r is already listed", msg, "already listed")

	te.runner(1, "https://github.com/o/r")
	if _, err := te.e.RemoveTarget("o/r"); err == nil {
		t.Error("target with runners removed")
	}
	os.RemoveAll(te.e.runnerDir(1))
	te.e.want["o+r"] = 3
	msg, err = te.e.RemoveTarget("o/r")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "removed o/r", msg, "removed")
	_, ok := te.e.want["o+r"]
	eq(t, false, ok, "want dropped")
}
