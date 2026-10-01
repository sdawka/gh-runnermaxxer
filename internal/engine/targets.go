package engine

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// A target is a repository or organization that runners register against.
// The list lives in .runnermaxxer.targets, one per line, as owner/repo,
// org-name, or a full https://github.com/... URL, optionally followed by
// autoscale bounds ("min=1 max=5"). Blank lines and '#' comments are kept.

var (
	reRepoURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	reOrgURL  = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+$`)
	reDigits  = regexp.MustCompile(`^[0-9]+$`)
)

const ghPrefix = "https://github.com/"

// normalizeURL maps SSH clone URLs and strips trailing slashes and a .git
// suffix so pasted URLs validate.
func normalizeURL(u string) string {
	switch {
	case strings.HasPrefix(u, "git@github.com:"):
		u = ghPrefix + strings.TrimPrefix(u, "git@github.com:")
	case strings.HasPrefix(u, "ssh://git@github.com/"):
		u = ghPrefix + strings.TrimPrefix(u, "ssh://git@github.com/")
	}
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimSuffix(u, "/")
	return u
}

func validRepoURL(u string) bool   { return reRepoURL.MatchString(u) }
func validOrgURL(u string) bool    { return reOrgURL.MatchString(u) }
func validTargetURL(u string) bool { return validRepoURL(u) || validOrgURL(u) }

// validPrefix: at most 60 characters, since GitHub caps runner names at 64
// and "-NNN" follows.
func validPrefix(p string) bool { return rePrefix.MatchString(p) }

// targetEntryToURL expands a targets-file entry to a full URL ("" for
// blanks and comments).
func targetEntryToURL(e string) string {
	e = strings.TrimSpace(e)
	if e == "" || strings.HasPrefix(e, "#") {
		return ""
	}
	switch {
	case strings.HasPrefix(e, ghPrefix):
	case strings.HasPrefix(e, "git@github.com:"), strings.HasPrefix(e, "ssh://git@github.com/"):
	case strings.HasPrefix(e, "http://github.com/"):
		e = "https://" + strings.TrimPrefix(e, "http://")
	case strings.HasPrefix(e, "github.com/"):
		e = "https://" + e
	default:
		e = ghPrefix + e
	}
	return normalizeURL(e)
}

// targetLine is one parsed targets-file line.
type targetLine struct {
	entry     string // "" for blank/comment lines
	hasBounds bool
	min, max  int
}

// splitTargetLine parses "owner/repo min=1 max=5 # comment". A missing min
// defaults to 0, a missing max to maxRunners. ok is false when the suffix
// is malformed (unknown key, non-numeric value, min > max).
func splitTargetLine(line string, maxRunners int) (targetLine, bool) {
	var tl targetLine
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return tl, true
	}
	fields := strings.Fields(line)
	tl.entry = fields[0]
	mn, mx := -1, -1
	has := false
	for _, tok := range fields[1:] {
		if strings.HasPrefix(tok, "#") {
			break
		}
		k, v, found := strings.Cut(tok, "=")
		if !found || !reDigits.MatchString(v) {
			return tl, false
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return tl, false
		}
		switch k {
		case "min":
			mn = n
		case "max":
			mx = n
		default:
			return tl, false
		}
		has = true
	}
	if !has {
		return tl, true
	}
	if mn < 0 {
		mn = 0
	}
	if mx < 0 {
		mx = maxRunners
	}
	if mn > mx {
		return tl, false
	}
	tl.hasBounds, tl.min, tl.max = true, mn, mx
	return tl, true
}

func targetType(u string) string {
	if validRepoURL(u) {
		return "repo"
	}
	return "org"
}

func targetAPIEndpoint(u string) string {
	p := strings.TrimPrefix(u, ghPrefix)
	if targetType(u) == "repo" {
		return "repos/" + p + "/actions/runners"
	}
	return "orgs/" + p + "/actions/runners"
}

// targetLabel is the short display form: owner/repo, or "org (organization)".
func targetLabel(u string) string {
	p := strings.TrimPrefix(u, ghPrefix)
	if validOrgURL(u) {
		return p + " (organization)"
	}
	return p
}

// sameTarget is a case-insensitive URL match (GitHub names are
// case-insensitive); an empty first argument never matches.
func sameTarget(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

// targetKey is a lowercased owner/repo with '/' -> '+' (never collides,
// '+' can't appear in GitHub names).
func targetKey(u string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(u, ghPrefix)), "/", "+")
}

// readLines returns a file's lines (no trailing empty element), nil when
// the file is missing.
func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(data), "\n")
	return strings.Split(s, "\n")
}

func writeLines(path string, lines []string) error {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return writeFileAtomic(path, []byte(b.String()), 0o644)
}

// targetsFile wraps .runnermaxxer.targets.
type targetsFile struct {
	path       string
	maxRunners func() int
}

// load lists valid target URLs in file order (not deduplicated).
func (f targetsFile) load() []string {
	var out []string
	for _, line := range readLines(f.path) {
		tl, ok := splitTargetLine(strings.TrimSuffix(line, "\r"), f.maxRunners())
		if !ok {
			continue
		}
		u := targetEntryToURL(tl.entry)
		if u != "" && validTargetURL(u) {
			out = append(out, u)
		}
	}
	return out
}

// invalid lists lines that are neither blank, comments nor valid targets.
func (f targetsFile) invalid() []string {
	var out []string
	for _, line := range readLines(f.path) {
		line = strings.TrimSuffix(line, "\r")
		tl, ok := splitTargetLine(line, f.maxRunners())
		u := ""
		if ok {
			u = targetEntryToURL(tl.entry)
			if u == "" {
				continue
			}
		}
		if !validTargetURL(u) {
			out = append(out, line)
		}
	}
	return out
}

func (f targetsFile) contains(u string) bool {
	for _, t := range f.load() {
		if sameTarget(t, u) {
			return true
		}
	}
	return false
}

// add appends a target (as owner/repo or org) unless already listed.
func (f targetsFile) add(u string) error {
	if f.contains(u) {
		return nil
	}
	data, err := os.ReadFile(f.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var b strings.Builder
	if os.IsNotExist(err) {
		b.WriteString("# Targets offered at startup (owner/repo, org, or URL; one per line)\n")
	} else {
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	b.WriteString(strings.TrimPrefix(u, ghPrefix))
	b.WriteByte('\n')
	return writeFileAtomic(f.path, []byte(b.String()), 0o644)
}

// remove drops every line that resolves to the target; comments and blank
// lines are kept.
func (f targetsFile) remove(u string) error {
	lines := readLines(f.path)
	if lines == nil {
		if _, err := os.Stat(f.path); err != nil {
			return nil
		}
	}
	var out []string
	for _, line := range lines {
		tl, _ := splitTargetLine(strings.TrimSuffix(line, "\r"), f.maxRunners())
		if v := targetEntryToURL(tl.entry); v != "" && sameTarget(v, u) {
			continue
		}
		out = append(out, line)
	}
	return writeLines(f.path, out)
}

// bounds returns a target's autoscale bounds from its first entry.
func (f targetsFile) bounds(u string) (mn, mx int, ok bool) {
	for _, line := range readLines(f.path) {
		tl, valid := splitTargetLine(strings.TrimSuffix(line, "\r"), f.maxRunners())
		if !valid {
			continue
		}
		v := targetEntryToURL(tl.entry)
		if v == "" || !sameTarget(v, u) {
			continue
		}
		return tl.min, tl.max, tl.hasBounds
	}
	return 0, 0, false
}

// setBounds sets (set=true) or clears a target's bounds, rewriting the first
// line naming it in place (its trailing comment is dropped); the target is
// appended first if it isn't listed.
func (f targetsFile) setBounds(u string, set bool, mn, mx int) error {
	if err := f.add(u); err != nil {
		return err
	}
	lines := readLines(f.path)
	hit := false
	for i, line := range lines {
		if hit {
			break
		}
		tl, ok := splitTargetLine(strings.TrimSuffix(line, "\r"), f.maxRunners())
		if !ok {
			continue
		}
		v := targetEntryToURL(tl.entry)
		if v == "" || !sameTarget(v, u) {
			continue
		}
		if set {
			lines[i] = tl.entry + " min=" + strconv.Itoa(mn) + " max=" + strconv.Itoa(mx)
		} else {
			lines[i] = tl.entry
		}
		hit = true
	}
	return writeLines(f.path, lines)
}
