package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type App struct {
	token string
	port  int

	mu   sync.Mutex
	scan *Scan
	gen  int
}

func (a *App) startScan(path string) (*Scan, error) {
	p, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scan != nil {
		a.scan.cancelled.Store(true)
	}
	a.gen++
	s := newScan(p, a.gen)
	a.scan = s
	go s.run()
	return s, nil
}

func (a *App) current() *Scan {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scan
}

func (a *App) result(w http.ResponseWriter) *Result {
	if s := a.current(); s != nil {
		if r := s.result.Load(); r != nil {
			return r
		}
	}
	httpErr(w, http.StatusConflict, "no finished scan yet")
	return nil
}

// guard blocks DNS-rebinding (Host check) and cross-site requests (token header,
// which browsers can't attach cross-origin without a CORS preflight we never answer).
func (a *App) guard(next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", a.port): true,
		fmt.Sprintf("localhost:%d", a.port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Token")), []byte(a.token)) != 1 {
				httpErr(w, http.StatusUnauthorized, "bad token — open the link printed in the terminal")
				return
			}
			h.Set("Cache-Control", "no-store")
		} else {
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/prefs", a.handlePrefsGet)
	mux.HandleFunc("POST /api/prefs", a.handlePrefsSet)
	mux.HandleFunc("POST /api/scan", a.handleScan)
	mux.HandleFunc("POST /api/cancel", a.handleCancel)
	mux.HandleFunc("GET /api/node", a.handleNode)
	mux.HandleFunc("GET /api/top", a.handleTop)
	mux.HandleFunc("GET /api/types", a.handleTypes)
	mux.HandleFunc("GET /api/hints", a.handleHints)
	mux.HandleFunc("POST /api/reveal", a.handleReveal)
	mux.HandleFunc("POST /api/trash", a.handleTrash)
	mux.HandleFunc("POST /api/pick", a.handlePick)
	mux.HandleFunc("POST /api/script", a.handleScript)
	mux.HandleFunc("POST /api/fda", a.handleFDA)
	mux.HandleFunc("POST /api/untrash", a.handleUntrash)
	mux.HandleFunc("GET /api/find", a.handleFind)
	mux.HandleFunc("GET /api/volume", a.handleVolume)
	mux.HandleFunc("POST /api/rescan", a.handleRescan)
	mux.HandleFunc("POST /api/trash-batch", a.handleTrashBatch)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type idReq struct {
	ID int `json:"id"`
}

func readID(r *http.Request) (int, error) {
	var req idReq
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		return 0, err
	}
	return req.ID, nil
}

type NodeOut struct {
	ID    int32     `json:"id"`
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Files int64     `json:"files"`
	Mod   int64     `json:"mod"`
	Dir   bool      `json:"dir"`
	Cat   string    `json:"cat,omitempty"`
	Err   bool      `json:"err,omitempty"`
	Skip  bool      `json:"skip,omitempty"`
	Prot  int       `json:"prot,omitempty"` // protSoft / protHard
	Kids  []NodeOut `json:"kids,omitempty"`
}

func out(n *Node) NodeOut {
	o := NodeOut{ID: n.ID, Name: n.Name, Size: n.Size, Files: n.Files, Mod: n.Mod, Dir: n.IsDir, Err: n.Err, Skip: n.Skipped}
	if !n.IsDir {
		o.Cat = categoryOf(extOf(n.Name), false)
	}
	return o
}

type volume struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
}

func volumeOf(path string) *volume {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	bs := uint64(st.Bsize)
	return &volume{Total: int64(uint64(st.Blocks) * bs), Free: int64(uint64(st.Bavail) * bs)}
}

// ---- handlers ----

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	s := a.current()
	if s == nil {
		writeJSON(w, map[string]any{"state": "idle", "home": home})
		return
	}
	state, fatal, samples := s.getState()
	cur := ""
	if p := s.current.Load(); p != nil {
		cur = *p
	}
	resp := map[string]any{
		"state":      state,
		"home":       home,
		"error":      fatal,
		"gen":        s.Gen,
		"root":       s.Root,
		"files":      s.files.Load(),
		"dirs":       s.dirs.Load(),
		"bytes":      s.bytes.Load(),
		"errors":     s.errs.Load(),
		"errSamples": samples,
		"current":    cur,
		"elapsedMs":  s.elapsed().Milliseconds(),
		"volume":     volumeOf(s.Root),
	}
	if state == "scanning" && r.URL.Query().Get("live") == "1" {
		resp["live"] = s.liveSnapshot()
	}
	if res := s.result.Load(); res != nil {
		res.mu.RLock()
		resp["bytes"] = res.Root.Size
		resp["files"] = res.Root.Files
		res.mu.RUnlock()
	}
	writeJSON(w, resp)
}

func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Path == "" {
		httpErr(w, http.StatusBadRequest, "path required")
		return
	}
	p, err := expandPath(req.Path)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		httpErr(w, http.StatusBadRequest, "not a readable folder: "+p)
		return
	}
	s, err := a.startScan(p)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"gen": s.Gen, "root": s.Root})
}

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	if s := a.current(); s != nil {
		s.cancelled.Store(true)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleNode(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	res.mu.RLock()
	defer res.mu.RUnlock()
	n := res.get(id)
	if n == nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}

	type crumb struct {
		ID   int32  `json:"id"`
		Name string `json:"name"`
	}
	var crumbs []crumb
	for p := n; p != nil; p = p.Parent {
		crumbs = append(crumbs, crumb{p.ID, p.Name})
	}
	for i, j := 0, len(crumbs)-1; i < j; i, j = i+1, j-1 {
		crumbs[i], crumbs[j] = crumbs[j], crumbs[i]
	}

	const maxKids, maxNested, maxGrand = 2000, 250, 48
	kids := n.Children
	var moreCount int
	var moreSize int64
	if len(kids) > maxKids {
		for _, c := range kids[maxKids:] {
			moreCount++
			moreSize += c.Size
		}
		kids = kids[:maxKids]
	}
	nodePath := n.Path()
	children := make([]NodeOut, 0, len(kids))
	for i, c := range kids {
		o := out(c)
		o.Prot = protection(joinPath(nodePath, c.Name))
		if c.IsDir && i < maxNested && c.Size > 0 {
			g := c.Children
			if len(g) > maxGrand {
				g = g[:maxGrand]
			}
			for _, gc := range g {
				if gc.Size > 0 {
					o.Kids = append(o.Kids, out(gc))
				}
			}
		}
		children = append(children, o)
	}

	self := out(n)
	writeJSON(w, map[string]any{
		"node":     self,
		"path":     nodePath,
		"crumbs":   crumbs,
		"children": children,
		"more":     moreCount,
		"moreSize": moreSize,
	})
}

func (a *App) handleTop(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	res.mu.RLock()
	defer res.mu.RUnlock()
	type row struct {
		NodeOut
		Folder   string `json:"folder"`
		ParentID int32  `json:"parentId"`
	}
	rows := []row{}
	for _, n := range res.Top {
		if n.isGone() {
			continue
		}
		rows = append(rows, row{out(n), n.Parent.Path(), n.Parent.ID})
		if len(rows) == 200 {
			break
		}
	}
	writeJSON(w, rows)
}

func (a *App) handleTypes(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	res.mu.RLock()
	defer res.mu.RUnlock()
	type kv struct {
		Name string `json:"name"`
		Cat  string `json:"cat,omitempty"`
		Stat
	}
	cats := []kv{}
	for k, v := range res.Cats {
		if v.Count > 0 {
			cats = append(cats, kv{Name: k, Stat: *v})
		}
	}
	exts := []kv{}
	for k, v := range res.Exts {
		if v.Count > 0 {
			cat := "Other"
			if k != "(none)" {
				cat = categoryOf(k[1:], false)
			}
			exts = append(exts, kv{Name: k, Cat: cat, Stat: *v})
		}
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].Size > cats[j].Size })
	sort.Slice(exts, func(i, j int) bool { return exts[i].Size > exts[j].Size })
	if len(exts) > 80 {
		exts = exts[:80]
	}
	writeJSON(w, map[string]any{"cats": cats, "exts": exts})
}

func (a *App) handleHints(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	res.mu.RLock()
	defer res.mu.RUnlock()
	type row struct {
		NodeOut
		Path  string `json:"path"`
		Label string `json:"label"`
		Desc  string `json:"desc"`
		Kind  string `json:"kind,omitempty"` // "cmd": prefer the command; "review": keep what you need
	}
	rows := []row{}
	for _, h := range res.Hints {
		if h.Node.isGone() || h.Node.Size < minHintSize {
			continue
		}
		o := out(h.Node)
		p := h.Node.Path()
		o.Prot = protection(p)
		rows = append(rows, row{o, p, h.Label, h.Desc, hintKind[h.Label]})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Size > rows[j].Size })
	if len(rows) > 300 {
		rows = rows[:300]
	}
	writeJSON(w, rows)
}

func (a *App) nodePath(w http.ResponseWriter, r *http.Request) (*Result, *Node, string) {
	res := a.result(w)
	if res == nil {
		return nil, nil, ""
	}
	id, err := readID(r)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "bad request")
		return nil, nil, ""
	}
	res.mu.RLock()
	n := res.get(id)
	var p string
	if n != nil {
		p = n.Path()
	}
	res.mu.RUnlock()
	if n == nil {
		httpErr(w, http.StatusNotFound, "not found")
		return nil, nil, ""
	}
	return res, n, p
}

func (a *App) handleReveal(w http.ResponseWriter, r *http.Request) {
	_, _, p := a.nodePath(w, r)
	if p == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", p)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(p))
	}
	if err := cmd.Run(); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handlePick shows the system folder picker (browser mode; the app window uses NSOpenPanel).
func (a *App) handlePick(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "darwin" {
		httpErr(w, http.StatusNotImplemented, "folder picker is only available on macOS — type a path instead")
		return
	}
	script := `POSIX path of (choose folder with prompt "Choose a folder to scan" default location (path to home folder))`
	outp, err := exec.Command("osascript", "-e", script).Output()
	if err != nil { // includes the user pressing Cancel
		writeJSON(w, map[string]string{"path": ""})
		return
	}
	p := strings.TrimSpace(string(outp))
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	writeJSON(w, map[string]string{"path": p})
}

func isDarwin() bool { return runtime.GOOS == "darwin" }

func execOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
