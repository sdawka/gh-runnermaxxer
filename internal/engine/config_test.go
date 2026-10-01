package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vals(s *settings) string {
	var out []string
	for _, k := range configKeys {
		out = append(out, s.vals[k])
	}
	return strings.Join(out, " ")
}

func TestConfigSampleParsesCleanly(t *testing.T) {
	s := newSettings(nil)
	if err := s.parseFile(filepath.Join("..", "..", ".runnermaxxer.conf.sample")); err != nil {
		t.Fatal(err)
	}
	eq(t, "my-runner 20 5 5 10 12 1 0 0 10", vals(s), "every key loaded")
	eq(t, 0, len(s.warnings), "no warnings")
}

func TestConfigHostileLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".runnermaxxer.conf")
	content := `MAX_RUNNERS="5" # note
AUTOSCALE=1
RUNNER_NAME_PREFIX='x-y'
EVIL=$(touch pwned)
REFRESH_INTERVAL=abc
RUNNER_BASE_DIR=/x
GH_HEALTH_TICKS="$(touch pwned)"
MAX_LOG_SIZE_MB="3"; touch pwned
touch pwned
   # indented comment
NOPE=1
EPHEMERAL_RUNNERS="07"
AUTOSCALE_IDLE_MINUTES="4"` + "\r\n"
	os.WriteFile(p, []byte(content), 0o644)
	s := newSettings(nil)
	s.parseFile(p)
	cases := []struct{ key, want, name string }{
		{"MAX_RUNNERS", "5", "double-quoted value with a trailing comment"},
		{"AUTOSCALE", "1", "bare value"},
		{"RUNNER_NAME_PREFIX", "x-y", "single-quoted value"},
		{"REFRESH_INTERVAL", "5", "invalid value keeps the default"},
		{"GH_HEALTH_TICKS", "12", "command substitution in quotes rejected"},
		{"MAX_LOG_SIZE_MB", "10", "trailing command after the value rejects the line"},
		{"EPHEMERAL_RUNNERS", "0", "07 is not a bool"},
		{"AUTOSCALE_IDLE_MINUTES", "4", "CRLF line endings tolerated"},
	}
	for _, c := range cases {
		eq(t, c.want, s.vals[c.key], c.name)
	}
	w := strings.Join(s.warnings, "\n")
	for _, sub := range []string{
		"line 4 (EVIL) ignored",
		"ignored unknown key NOPE",
		"ignored invalid REFRESH_INTERVAL='abc'",
		"seconds, 1-3600",
		"set RUNNER_BASE_DIR in the environment",
		"not KEY=VALUE",
	} {
		contains(t, w, sub, "warning")
	}
}

func TestConfigSet(t *testing.T) {
	s := newSettings(nil)
	bad := []struct{ k, v string }{
		{"MAX_RUNNERS", "0"},
		{"RUNNER_NAME_PREFIX", strings.Repeat("a", 61)},
		{"EVIL", "1"},
		{"RUNNER_BASE_DIR", "/x"},
		{"RUNNER_NAME_PREFIX", "a$b"},
		{"AUTOSCALE", "2"},
	}
	for _, c := range bad {
		if err := s.set(c.k, c.v); err == nil {
			t.Errorf("set %s=%q should fail", c.k, c.v)
		}
	}
	if err := s.set("RUNNER_NAME_PREFIX", strings.Repeat("a", 60)); err != nil {
		t.Errorf("60-char prefix: %v", err)
	}
	if err := s.set("GH_HEALTH_TICKS", "0"); err != nil {
		t.Errorf("GH_HEALTH_TICKS 0: %v", err)
	}
	eq(t, "0", s.vals["GH_HEALTH_TICKS"], "set assigns")
	s.set("MAX_RUNNERS", "007")
	eq(t, "7", s.vals["MAX_RUNNERS"], "leading zeros stripped")
	err := s.set("REFRESH_INTERVAL", "0")
	if err == nil {
		t.Fatal("REFRESH_INTERVAL 0 should fail")
	}
	contains(t, err.Error(), "seconds, 1-3600", "error carries the rule")
}

func TestConfigSanitize(t *testing.T) {
	s := newSettings(nil)
	s.vals["REFRESH_INTERVAL"] = "99999"
	s.vals["MAX_RUNNERS"] = "0"
	s.vals["GH_HEALTH_TICKS"] = "x"
	s.vals["AUTOSCALE"] = "5"
	s.sanitize()
	eq(t, "3600", s.vals["REFRESH_INTERVAL"], "clamped high")
	eq(t, "1", s.vals["MAX_RUNNERS"], "clamped low")
	eq(t, "12", s.vals["GH_HEALTH_TICKS"], "non-number reset")
	eq(t, "0", s.vals["AUTOSCALE"], "bad bool reset")
	contains(t, strings.Join(s.warnings, "\n"), "REFRESH_INTERVAL clamped to 3600", "clamp noted")
	s.vals["REFRESH_INTERVAL"] = "99999"
	s.sanitize()
	n := 0
	for _, w := range s.warnings {
		if strings.Contains(w, "REFRESH_INTERVAL clamped") {
			n++
		}
	}
	eq(t, 1, n, "no duplicate note")
}

func TestConfigSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".runnermaxxer.conf")
	s := newSettings(nil)
	for k, v := range map[string]string{
		"RUNNER_NAME_PREFIX": "rt-host", "MAX_RUNNERS": "33", "REFRESH_INTERVAL": "7",
		"MAX_RESTART_ATTEMPTS": "4", "MAX_LOG_SIZE_MB": "12", "GH_HEALTH_TICKS": "0",
		"SHARED_TOOL_CACHE": "0", "EPHEMERAL_RUNNERS": "1", "AUTOSCALE": "1", "AUTOSCALE_IDLE_MINUTES": "3",
	} {
		s.vals[k] = v
	}
	if err := s.save(p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	contains(t, string(data), "\nMAX_RUNNERS=\"33\"\n", "KEY=\"VALUE\"")
	s2 := newSettings(nil)
	s2.parseFile(p)
	eq(t, vals(s), vals(s2), "round trip")
	eq(t, 0, len(s2.warnings), "no warnings")
	s.vals["RUNNER_NAME_PREFIX"] = `a"b$(x)`
	s.save(p)
	data, _ = os.ReadFile(p)
	contains(t, string(data), "\nRUNNER_NAME_PREFIX=\"\"\n", "quote-breaking value never written")
}

func TestConfigEnvOverride(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"9\"\n")
	te.env["REFRESH_INTERVAL"] = "11"
	te.env["MAX_RUNNERS"] = "3"
	te.e.loadConfig()
	eq(t, "11", te.e.cfg.vals["REFRESH_INTERVAL"], "env overrides the default")
	eq(t, "9", te.e.cfg.vals["MAX_RUNNERS"], "the file wins over env")
	eq(t, false, te.e.needsSetup, "config exists")
}

func TestConfigLegacyMigration(t *testing.T) {
	te := newTestEnv(t, "REPO_URL=\"https://github.com/o/legacy.git\"\nMAX_RUNNERS=\"3\"\n")
	data, _ := os.ReadFile(te.e.targets.path)
	contains(t, string(data), "\no/legacy\n", "moved to the targets file")
	conf, _ := os.ReadFile(te.e.confPath)
	if strings.Contains(string(conf), "REPO_URL") {
		t.Error("REPO_URL still in the config")
	}
	eq(t, "3", te.e.cfg.vals["MAX_RUNNERS"], "other keys kept")
}

func TestConfigReloadIfChanged(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"3\"\n")
	e := te.e
	sig := e.confSig
	e.reloadConfigIfChanged()
	eq(t, sig, e.confSig, "unchanged -> no reload")
	os.WriteFile(e.confPath, []byte("MAX_RUNNERS=\"4\"\n# changed\n"), 0o644)
	e.reloadConfigIfChanged()
	eq(t, "4", e.cfg.vals["MAX_RUNNERS"], "changed config reloaded")
	te.targets("o/r\n")
	e.reloadConfigIfChanged()
	eq(t, e.fileSigs(), e.confSig, "targets change recorded")
}

func TestNeedsSetupAndSetup(t *testing.T) {
	te := newTestEnv(t, "")
	e := te.e
	eq(t, true, e.needsSetup, "no config -> needs setup")
	if _, err := e.Scale(map[string]int{"o/r": 1}); err != ErrNeedsSetup {
		t.Errorf("Scale before setup: %v", err)
	}
	if _, err := e.Setup("bad prefix!", 5); err == nil {
		t.Error("invalid prefix accepted")
	}
	if _, err := e.Setup("host", 0); err == nil {
		t.Error("max 0 accepted")
	}
	msg, err := e.Setup("my-host", 7)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, msg, ".runnermaxxer.conf", "message")
	eq(t, false, e.needsSetup, "set up")
	data, _ := os.ReadFile(e.confPath)
	contains(t, string(data), `RUNNER_NAME_PREFIX="my-host"`, "prefix saved")
	contains(t, string(data), `MAX_RUNNERS="7"`, "max saved")
}

func TestSetConfig(t *testing.T) {
	te := newTestEnv(t, "MAX_RUNNERS=\"3\"\n")
	msg, err := te.e.SetConfig("REFRESH_INTERVAL", "07")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "REFRESH_INTERVAL=7", msg, "message")
	data, _ := os.ReadFile(te.e.confPath)
	contains(t, string(data), `REFRESH_INTERVAL="7"`, "saved")
	msg, _ = te.e.SetConfig("MAX_RUNNERS", "900")
	contains(t, msg, "MAX_RUNNERS=500", "clamped value")
	contains(t, msg, "Note: MAX_RUNNERS clamped to 500", "clamp note")
	if _, err := te.e.SetConfig("EVIL", "1"); err == nil {
		t.Error("unknown key accepted")
	}
}
