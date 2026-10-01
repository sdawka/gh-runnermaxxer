package engine

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// platform is what detect_platform worked out.
type platform struct {
	osFamily   string // macos | linux | unknown
	runnerOS   string // osx | linux (tarball naming)
	runnerArch string // arm64 | x64
	isWSL      bool
	notes      []string // non-fatal oddities (unknown OS/arch)
}

// detectPlatform ports detect_platform. Hosts the runner can't run on at all
// (Windows, WSL1, musl) are an error.
func detectPlatform() (platform, error) {
	var p platform
	switch runtime.GOOS {
	case "darwin":
		p.osFamily, p.runnerOS = "macos", "osx"
	case "linux":
		p.osFamily, p.runnerOS = "linux", "linux"
		if v, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(v)), "microsoft") {
			p.isWSL = true
			if out, _ := exec.Command("uname", "-r").Output(); strings.HasSuffix(strings.TrimSpace(string(out)), "-Microsoft") {
				return p, errors.New("WSL1 detected - the GitHub runner requires WSL2 or native Linux")
			}
		}
		// The runner is dotnet-based and requires glibc
		if out, _ := exec.Command("ldd", "--version").CombinedOutput(); strings.Contains(strings.ToLower(string(out)), "musl") {
			return p, errors.New("musl libc detected (Alpine?) - the GitHub runner requires glibc and cannot run here")
		}
	case "windows":
		return p, errors.New("windows is not supported - the Windows runner uses a .zip and config.cmd. See https://github.com/actions/runner")
	default:
		p.osFamily, p.runnerOS = "unknown", "linux"
		p.notes = append(p.notes, "unknown OS '"+runtime.GOOS+"' - assuming Linux; things may not work")
	}
	switch runtime.GOARCH {
	case "arm64":
		p.runnerArch = "arm64"
	case "amd64":
		p.runnerArch = "x64"
	default:
		p.runnerArch = "x64"
		p.notes = append(p.notes, "unknown architecture '"+runtime.GOARCH+"' - assuming x64")
	}
	// A Rosetta-translated binary on Apple Silicon reports amd64; the native
	// arch is what matters for the runner binary.
	if p.osFamily == "macos" && p.runnerArch == "x64" {
		if out, err := exec.Command("sysctl", "-in", "sysctl.proc_translated").Output(); err == nil && strings.TrimSpace(string(out)) == "1" {
			p.runnerArch = "arm64"
		}
	}
	return p, nil
}

// DefaultPrefix is the runner name prefix Setup uses when given none.
func DefaultPrefix() string { return truncRunes(defaultHostname(), 60) }

// defaultHostname is the short host name, sanitized to characters GitHub
// accepts in runner names.
func defaultHostname() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	var b strings.Builder
	for _, r := range h {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "runner"
	}
	return b.String()
}

func memGB(osFamily string) int {
	if osFamily == "macos" {
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return int(n / 1073741824)
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == "MemTotal:" {
			n, _ := strconv.ParseInt(fs[1], 10, 64)
			return int(n / 1048576)
		}
	}
	return 0
}

func haveCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// detectLabels ports detect_labels: best-effort, every probe may fail.
// Returns space-separated labels.
func detectLabels(p platform) string {
	var labels []string
	switch p.osFamily {
	case "macos":
		labels = append(labels, "macos")
	case "linux":
		labels = append(labels, "linux")
		if id := osReleaseID(); id != "" {
			labels = append(labels, id)
		}
		if p.isWSL {
			labels = append(labels, "wsl")
		}
	}
	switch p.runnerArch {
	case "arm64":
		labels = append(labels, "arm64")
		if p.osFamily == "macos" {
			labels = append(labels, "apple-silicon")
		}
	case "x64":
		labels = append(labels, "x64")
	}
	if p.osFamily == "macos" {
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			if v := strings.SplitN(strings.TrimSpace(string(out)), ".", 2)[0]; v != "" {
				labels = append(labels, "macos-"+v)
			}
		}
	}
	if haveCmd("docker") {
		labels = append(labels, "docker")
	}
	if p.osFamily == "macos" {
		if out, err := exec.Command("system_profiler", "SPDisplaysDataType").Output(); err == nil && strings.Contains(string(out), "Metal") {
			labels = append(labels, "metal")
		}
	} else if haveCmd("nvidia-smi") {
		labels = append(labels, "gpu", "nvidia")
	}
	mem := memGB(p.osFamily)
	if mem >= 16 {
		labels = append(labels, "high-memory")
	}
	if mem >= 32 {
		labels = append(labels, "32gb-ram")
	}
	if runtime.NumCPU() >= 8 {
		labels = append(labels, "8-core")
	}
	return strings.Join(labels, " ")
}

func osReleaseID() string {
	for _, line := range readLines("/etc/os-release") {
		if v, ok := strings.CutPrefix(line, "ID="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// diskFree reports free MB and percent free of the filesystem holding dir
// (df -Pk: available / size). freeMB is 999999 and pct -1 when unknown.
func diskFree(dir string) (freeMB int, pct int) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 999999, -1
	}
	bsize := uint64(st.Bsize)
	avail := uint64(st.Bavail) * bsize
	size := uint64(st.Blocks) * bsize
	freeMB = int(avail / (1024 * 1024))
	pct = -1
	if size > 0 {
		pct = int(avail * 100 / size)
	}
	return freeMB, pct
}
