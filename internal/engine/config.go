package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sdawka/gh-runnermaxxer/internal/state"
)

// configKeys are the settings a .runnermaxxer.conf may set, in the order
// save writes them (runnermaxxer.sh CONFIG_KEYS).
var configKeys = []string{
	"RUNNER_NAME_PREFIX", "MAX_RUNNERS", "REFRESH_INTERVAL", "MAX_RESTART_ATTEMPTS",
	"MAX_LOG_SIZE_MB", "GH_HEALTH_TICKS", "SHARED_TOOL_CACHE", "EPHEMERAL_RUNNERS",
	"AUTOSCALE", "AUTOSCALE_IDLE_MINUTES",
}

// legacyKeys are v2 single-target settings, migrated into the targets file.
var legacyKeys = []string{"REPO_URL", "ORG_URL"}

var configDefaults = map[string]string{
	"RUNNER_NAME_PREFIX":     "",
	"MAX_RUNNERS":            "20",
	"REFRESH_INTERVAL":       "5",
	"MAX_RESTART_ATTEMPTS":   "5",
	"MAX_LOG_SIZE_MB":        "10",
	"GH_HEALTH_TICKS":        "12",
	"SHARED_TOOL_CACHE":      "1",
	"EPHEMERAL_RUNNERS":      "0",
	"AUTOSCALE":              "0",
	"AUTOSCALE_IDLE_MINUTES": "10",
	"REPO_URL":               "",
	"ORG_URL":                "",
}

func isConfigKey(k string) bool {
	for _, c := range configKeys {
		if c == k {
			return true
		}
	}
	return false
}

func configKnownKey(k string) bool {
	if isConfigKey(k) {
		return true
	}
	for _, c := range legacyKeys {
		if c == k {
			return true
		}
	}
	return false
}

var (
	reNum9    = regexp.MustCompile(`^[0-9]{1,9}$`)
	reBool    = regexp.MustCompile(`^[01]$`)
	rePrefix  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,60}$`)
	reConfKV  = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=("([^"]*)"|'([^']*)'|([^\s#"']*))\s*(#.*)?$`)
	reConfNil = regexp.MustCompile(`^\s*(#.*)?$`)
	reIdent   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// configValidValue reports whether v is acceptable for k (config_valid_value).
// Upper bounds are clamped later by sanitize.
func configValidValue(k, v string) bool {
	if strings.ContainsAny(v, "\"'\\$`") {
		return false
	}
	switch k {
	case "RUNNER_NAME_PREFIX":
		return v == "" || rePrefix.MatchString(v)
	case "MAX_RUNNERS", "REFRESH_INTERVAL", "MAX_RESTART_ATTEMPTS", "MAX_LOG_SIZE_MB":
		if !reNum9.MatchString(v) {
			return false
		}
		n, _ := strconv.Atoi(v)
		return n >= 1
	case "GH_HEALTH_TICKS", "AUTOSCALE_IDLE_MINUTES":
		return reNum9.MatchString(v)
	case "SHARED_TOOL_CACHE", "EPHEMERAL_RUNNERS", "AUTOSCALE":
		return reBool.MatchString(v)
	case "REPO_URL", "ORG_URL":
		return v == "" || validTargetURL(normalizeURL(v))
	}
	return false
}

// configRule is the human rule for k, for error messages.
func configRule(k string) string {
	switch k {
	case "RUNNER_NAME_PREFIX":
		return "letters, digits, _ and -, at most 60 characters (GitHub caps runner names at 64)"
	case "MAX_RUNNERS":
		return "a whole number, 1-500"
	case "REFRESH_INTERVAL":
		return "seconds, 1-3600"
	case "MAX_RESTART_ATTEMPTS":
		return "a whole number, 1-100"
	case "MAX_LOG_SIZE_MB":
		return "megabytes, 1-10000"
	case "GH_HEALTH_TICKS":
		return "a whole number of ticks, 0 (off) to 100000"
	case "AUTOSCALE_IDLE_MINUTES":
		return "minutes, 0-100000"
	case "SHARED_TOOL_CACHE", "EPHEMERAL_RUNNERS", "AUTOSCALE":
		return "0 or 1"
	}
	return "one of: " + strings.Join(configKeys, " ")
}

// normNum strips leading zeros from a number ("07" -> "7").
func normNum(v string) string {
	if reNum9.MatchString(v) {
		n, _ := strconv.Atoi(v)
		return strconv.Itoa(n)
	}
	return v
}

// settings holds the raw configuration values plus the notes about them
// (ignored keys, clamped values) shown as snapshot warnings.
type settings struct {
	vals     map[string]string
	warnings []string
}

// newSettings starts from the defaults, overridden by non-empty environment
// variables of the same name (bash's VAR="${VAR:-default}").
func newSettings(getenv func(string) string) *settings {
	s := &settings{vals: map[string]string{}}
	for k, v := range configDefaults {
		s.vals[k] = v
		if getenv != nil {
			if ev := getenv(k); ev != "" {
				s.vals[k] = ev
			}
		}
	}
	return s
}

func (s *settings) warn(w string) {
	for _, x := range s.warnings {
		if x == w {
			return
		}
	}
	s.warnings = append(s.warnings, w)
}

// set is config_set: whitelist + validation, then assign.
func (s *settings) set(k, v string) error {
	if !isConfigKey(k) {
		return fmt.Errorf("unknown setting '%s' (settings: %s)", k, strings.Join(configKeys, " "))
	}
	if k != "RUNNER_NAME_PREFIX" {
		v = normNum(v)
	}
	if !configValidValue(k, v) {
		return fmt.Errorf("invalid value for %s: '%s' - %s", k, v, configRule(k))
	}
	s.vals[k] = v
	return nil
}

// parseFile assigns every valid KEY=VALUE line and warns about the rest
// (parse_config_file). A missing file is not an error.
func (s *settings) parseFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	name := filepath.Base(path)
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		n := i + 1
		line = strings.TrimSuffix(line, "\r")
		if reConfNil.MatchString(line) {
			continue
		}
		m := reConfKV.FindStringSubmatch(line)
		if m == nil {
			k := strings.TrimLeft(strings.SplitN(line, "=", 2)[0], " \t")
			if strings.Contains(line, "=") && reIdent.MatchString(k) {
				s.warn(fmt.Sprintf("%s line %d (%s) ignored: not KEY=VALUE (a plain or quoted value, no $ or commands)", name, n, k))
			} else {
				s.warn(fmt.Sprintf("%s line %d ignored: not KEY=VALUE", name, n))
			}
			continue
		}
		k := m[1]
		v := m[3] + m[4] + m[5]
		if k == "RUNNER_BASE_DIR" {
			s.warn(fmt.Sprintf("RUNNER_BASE_DIR in %s is ignored - set RUNNER_BASE_DIR in the environment, not the config file", name))
			continue
		}
		if !configKnownKey(k) {
			s.warn(fmt.Sprintf("ignored unknown key %s in %s", k, name))
			continue
		}
		if k != "RUNNER_NAME_PREFIX" {
			v = normNum(v)
		}
		if !configValidValue(k, v) {
			cur := s.vals[k]
			if cur == "" {
				cur = "the default"
			}
			s.warn(fmt.Sprintf("ignored invalid %s='%s' in %s (%s) - using %s", k, v, name, configRule(k), cur))
			continue
		}
		s.vals[k] = v
	}
	return nil
}

func (s *settings) clamp(k string, lo, hi, def int) {
	v := s.vals[k]
	if !reNum9.MatchString(v) {
		if v != "" {
			s.warn(fmt.Sprintf("%s='%s' is not a number - using %d", k, v, def))
		}
		s.vals[k] = strconv.Itoa(def)
		return
	}
	n, _ := strconv.Atoi(v)
	switch {
	case n < lo:
		s.warn(fmt.Sprintf("%s clamped to %d (was %d)", k, lo, n))
		n = lo
	case n > hi:
		s.warn(fmt.Sprintf("%s clamped to %d (was %d)", k, hi, n))
		n = hi
	}
	s.vals[k] = strconv.Itoa(n)
}

func (s *settings) boolean(k, def string) {
	if !reBool.MatchString(s.vals[k]) {
		s.warn(fmt.Sprintf("%s='%s' must be 0 or 1 - using %s", k, s.vals[k], def))
		s.vals[k] = def
	}
}

// sanitize resets what isn't a number and clamps what is out of range,
// noting each change (sanitize_settings).
func (s *settings) sanitize() {
	s.clamp("REFRESH_INTERVAL", 1, 3600, 5)
	s.clamp("MAX_RESTART_ATTEMPTS", 1, 100, 5)
	s.clamp("MAX_LOG_SIZE_MB", 1, 10000, 10)
	s.clamp("GH_HEALTH_TICKS", 0, 100000, 12)
	s.clamp("MAX_RUNNERS", 1, 500, 20)
	s.clamp("AUTOSCALE_IDLE_MINUTES", 0, 100000, 10)
	s.boolean("SHARED_TOOL_CACHE", "1")
	s.boolean("EPHEMERAL_RUNNERS", "0")
	s.boolean("AUTOSCALE", "0")
}

// validationWarnings are validate_config's non-fatal notes.
func (s *settings) validationWarnings() []string {
	var out []string
	if n, err := strconv.Atoi(s.vals["MAX_RUNNERS"]); err == nil && n > 100 {
		out = append(out, fmt.Sprintf("MAX_RUNNERS is very high (%d) - this may cause resource issues", n))
	}
	if s.vals["AUTOSCALE"] == "1" && s.vals["GH_HEALTH_TICKS"] == "0" {
		out = append(out, "AUTOSCALE=1 has no effect while GH_HEALTH_TICKS=0 (it runs after each GitHub poll)")
	}
	return out
}

// save writes every config key as KEY="VALUE" (save_config), atomically.
func (s *settings) save(path string) error {
	var b strings.Builder
	b.WriteString("# gh-runnermaxxer configuration\n")
	b.WriteString("# Projects (repos/orgs) are listed in .runnermaxxer.targets, not here.\n")
	for _, k := range configKeys {
		v := s.vals[k]
		// Never write a value that could break out of the quotes
		if strings.ContainsAny(v, "\"\\$`") {
			v = ""
		}
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, v)
	}
	return writeFileAtomic(path, []byte(b.String()), 0o644)
}

func (s *settings) int(k string) int {
	n, _ := strconv.Atoi(s.vals[k])
	return n
}

// typed is the effective (post-sanitize) configuration.
func (s *settings) typed() state.Config {
	return state.Config{
		RunnerNamePrefix:     s.vals["RUNNER_NAME_PREFIX"],
		MaxRunners:           s.int("MAX_RUNNERS"),
		RefreshInterval:      s.int("REFRESH_INTERVAL"),
		MaxRestartAttempts:   s.int("MAX_RESTART_ATTEMPTS"),
		MaxLogSizeMB:         s.int("MAX_LOG_SIZE_MB"),
		GHHealthTicks:        s.int("GH_HEALTH_TICKS"),
		SharedToolCache:      s.vals["SHARED_TOOL_CACHE"] == "1",
		EphemeralRunners:     s.vals["EPHEMERAL_RUNNERS"] == "1",
		Autoscale:            s.vals["AUTOSCALE"] == "1",
		AutoscaleIdleMinutes: s.int("AUTOSCALE_IDLE_MINUTES"),
	}
}

// writeFileAtomic writes via a temp file in the same directory + rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// fileSig is "mtime:size" of a file ("0:0" when missing).
func fileSig(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "0:0"
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}
