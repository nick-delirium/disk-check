package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Cleanup scripts: the UI sends picked node ids, the server turns them into
// shell commands for the user to copy and run. Nothing here deletes anything.

type scriptReq struct {
	IDs  []int  `json:"ids"`
	Mode string `json:"mode"` // "trash" (default) or "rm"
}

type scriptOut struct {
	Script  string `json:"script"`
	Count   int    `json:"count"`
	Bytes   int64  `json:"bytes"`
	Skipped int    `json:"skipped"`
	Live    []int  `json:"live"` // picked ids that still exist (the UI drops the rest)
}

// shellQuote wraps s in single quotes, which the shell never expands.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func humanBytes(b int64) string {
	if b < 1000 {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(b)
	i := -1
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, units[i])
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}

func buildScript(res *Result, req scriptReq) scriptOut {
	res.mu.RLock()
	defer res.mu.RUnlock()

	items := res.topPicks(req.IDs)
	live := []int{}
	for _, id := range req.IDs {
		if res.get(id) != nil {
			live = append(live, id)
		}
	}

	cmd := "trash"
	if req.Mode == "rm" {
		cmd = "rm -rf --"
	}
	// Paste-safe: one command per line and no comments. Interactive zsh does not
	// treat "#" as a comment by default, so a trailing "# 1.4 GB" would become
	// extra arguments to rm.
	var out scriptOut
	var b strings.Builder
	if cmd == "trash" {
		if _, err := os.Stat("/usr/bin/trash"); err != nil { // macOS < 15: go through Finder
			b.WriteString("trash() { osascript -e 'on run argv' -e 'repeat with p in argv' -e 'tell application \"Finder\" to delete (POSIX file (contents of p) as alias)' -e 'end repeat' -e 'end run' \"$@\" >/dev/null; }\n")
		}
	}
	for _, n := range items {
		p := n.Path()
		if n.Parent == nil || protection(p) != protNone {
			out.Skipped++
			continue
		}
		b.WriteString(cmd + " " + shellQuote(p) + "\n")
		out.Count++
		out.Bytes += n.Size
	}
	out.Script = b.String()
	out.Live = live
	return out
}

func readScriptReq(w http.ResponseWriter, r *http.Request) (scriptReq, bool) {
	var req scriptReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "bad request")
		return req, false
	}
	return req, true
}

// POST /api/script → script text plus the deduplicated count and size.
func (a *App) handleScript(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	req, ok := readScriptReq(w, r)
	if !ok {
		return
	}
	writeJSON(w, buildScript(res, req))
}

// POST /api/fda → opens the Full Disk Access pane in System Settings.
func (a *App) handleFDA(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "darwin" {
		httpErr(w, http.StatusNotImplemented, "macOS only")
		return
	}
	if err := exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run(); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
