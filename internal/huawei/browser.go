package huawei

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// OpenAuthBrowser opens the authorize URL in a brand-new private session
// (same approach as unified-gateway Cline OAuth):
// Windows Edge --inprivate / Chrome --incognito.
// Returns true when a private window was launched.
func OpenAuthBrowser(rawURL string) bool {
	if runtime.GOOS != "windows" || rawURL == "" {
		return false
	}
	for _, b := range browserProbe() {
		args := []string{"--new-window"}
		if b.modeFlag != "" {
			args = append(args, b.modeFlag)
		}
		args = append(args, rawURL)
		if err := exec.Command(b.exe, args...).Start(); err == nil {
			return true
		}
	}
	return false
}

type browserCandidate struct {
	exe      string
	modeFlag string
}

func browserProbe() []browserCandidate {
	var out []browserCandidate
	seen := map[string]bool{}
	add := func(exe, flag string) {
		if exe == "" || seen[exe] {
			return
		}
		if st, err := os.Stat(exe); err == nil && !st.IsDir() {
			seen[exe] = true
			out = append(out, browserCandidate{exe: exe, modeFlag: flag})
		}
	}
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	add(filepath.Join(pf86, "Microsoft", "Edge", "Application", "msedge.exe"), "--inprivate")
	add(filepath.Join(pf, "Microsoft", "Edge", "Application", "msedge.exe"), "--inprivate")
	add(filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"), "--incognito")
	add(filepath.Join(pf86, "Google", "Chrome", "Application", "chrome.exe"), "--incognito")
	if p := findOnPath("msedge.exe"); p != "" {
		add(p, "--inprivate")
	}
	if p := findOnPath("chrome.exe"); p != "" {
		add(p, "--incognito")
	}
	return out
}

func findOnPath(name string) string {
	for _, p := range strings.Split(os.Getenv("PATH"), ";") {
		f := filepath.Join(p, name)
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			return f
		}
	}
	return ""
}
