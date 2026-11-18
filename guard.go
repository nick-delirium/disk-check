package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Protection tiers for destructive actions.
const (
	protNone = 0
	protSoft = 1 // personal top-level folders: in-app Trash needs an explicit confirm; never in scripts or select-all
	protHard = 2 // system roots, home, its parents and ~/Library: never trashed, never in scripts
)

var hardRoots = map[string]bool{
	"/Applications": true, "/System": true, "/Library": true, "/Users": true, "/usr": true, "/bin": true,
	"/sbin": true, "/private": true, "/etc": true, "/var": true, "/opt": true, "/cores": true, "/Volumes": true,
}

var softHomeDirs = []string{"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music", "Public", "Applications"}

func protection(p string) int {
	if p == "/" || hardRoots[p] {
		return protHard
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return protNone
	}
	if p == home || strings.HasPrefix(home, p+"/") || p == home+"/Library" {
		return protHard
	}
	for _, d := range softHomeDirs {
		if p == home+"/"+d {
			return protSoft
		}
	}
	return protNone
}

// findPath returns the node for an absolute path inside this result, or nil.
func (r *Result) findPath(p string) *Node {
	root := r.Root
	if p == root.Name {
		return root
	}
	prefix := root.Name
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !strings.HasPrefix(p, prefix) {
		return nil
	}
	n := root
	for _, seg := range strings.Split(strings.TrimSuffix(p[len(prefix):], "/"), "/") {
		var next *Node
		for _, c := range n.Children {
			if c.Name == seg {
				next = c
				break
			}
		}
		if next == nil || !next.IsDir {
			return nil
		}
		n = next
	}
	if n.isGone() {
		return nil
	}
	return n
}

// GET /api/find?path=… → {"id": n} when the folder is part of the current scan.
func (a *App) handleFind(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	p, err := expandPath(r.URL.Query().Get("path"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res.mu.RLock()
	n := res.findPath(p)
	res.mu.RUnlock()
	if n == nil {
		httpErr(w, http.StatusNotFound, "not in this scan")
		return
	}
	writeJSON(w, map[string]int32{"id": n.ID})
}

// ---- trash + undo ----

type undoRec struct {
	res     *Result
	node    *Node
	orig    string
	trashed string
	at      time.Time
}

var (
	undoMu  sync.Mutex
	undoLog = map[string]undoRec{}
)

func rememberUndo(rec undoRec) string {
	b := make([]byte, 8)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	undoMu.Lock()
	defer undoMu.Unlock()
	for k, v := range undoLog { // keep it small: undo is offered for a few seconds
		if time.Since(v.at) > 10*time.Minute {
			delete(undoLog, k)
		}
	}
	undoLog[tok] = rec
	return tok
}

// POST /api/trash {id, force} → moves to Trash, returns an undo token when the
// Trash location is known.
func (a *App) handleTrash(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	var req struct {
		ID    int  `json:"id"`
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "bad request")
		return
	}
	res.mu.RLock()
	n := res.get(req.ID)
	var p string
	if n != nil {
		p = n.Path()
	}
	res.mu.RUnlock()
	switch {
	case n == nil:
		httpErr(w, http.StatusNotFound, "not found")
		return
	case n.Parent == nil:
		httpErr(w, http.StatusBadRequest, "can't trash the scanned folder itself")
		return
	}
	switch protection(p) {
	case protHard:
		httpErr(w, http.StatusForbidden, "protected folder")
		return
	case protSoft:
		if !req.Force {
			httpErr(w, http.StatusConflict, "personal folder, confirm first")
			return
		}
	}
	dest, err := moveToTrash(p)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		httpErr(w, http.StatusInternalServerError, "item is still there after trashing")
		return
	}
	res.mu.Lock()
	if !n.Gone {
		res.remove(n)
	}
	res.mu.Unlock()
	resp := map[string]any{"ok": true, "bytes": n.Size}
	if dest != "" {
		resp["undo"] = rememberUndo(undoRec{res: res, node: n, orig: p, trashed: dest, at: time.Now()})
	}
	writeJSON(w, resp)
}

// POST /api/untrash {undo} or {undos: [...]} → moves items back and restores them in the tree.
func (a *App) handleUntrash(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Undo  string   `json:"undo"`
		Undos []string `json:"undos"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "bad request")
		return
	}
	toks := req.Undos
	if req.Undo != "" {
		toks = append(toks, req.Undo)
	}
	restored, failed := 0, 0
	var lastErr string
	for _, tok := range toks {
		if err := untrash(tok); err != nil {
			failed++
			lastErr = err.Error()
		} else {
			restored++
		}
	}
	if restored == 0 {
		if lastErr == "" {
			lastErr = "nothing to undo"
		}
		httpErr(w, http.StatusConflict, lastErr)
		return
	}
	writeJSON(w, map[string]int{"restored": restored, "failed": failed})
}

func untrash(tok string) error {
	undoMu.Lock()
	rec, ok := undoLog[tok]
	delete(undoLog, tok)
	undoMu.Unlock()
	if !ok {
		return errors.New("nothing to undo")
	}
	if _, err := os.Lstat(rec.orig); err == nil {
		return errors.New("something already exists at " + rec.orig)
	}
	if err := os.Rename(rec.trashed, rec.orig); err != nil {
		return errors.New("couldn't put it back: " + err.Error())
	}
	rec.res.mu.Lock()
	rec.res.restore(rec.node)
	rec.res.mu.Unlock()
	return nil
}

// restore undoes Result.remove for a node that was put back from the Trash.
func (r *Result) restore(n *Node) {
	p := n.Parent
	if p == nil || !n.Gone || p.isGone() {
		return
	}
	n.Gone = false
	i := sort.Search(len(p.Children), func(i int) bool { return p.Children[i].Size < n.Size })
	p.Children = append(p.Children, nil)
	copy(p.Children[i+1:], p.Children[i:])
	p.Children[i] = n
	for a := p; a != nil; a = a.Parent {
		a.Size += n.Size
		a.Files += n.Files
	}
	r.adjustStats(n, 1)
}

// moveToTrash moves p to the Trash and returns where it went (empty if unknown).
func moveToTrash(p string) (string, error) {
	var out []byte
	var err error
	if _, statErr := os.Stat("/usr/bin/trash"); statErr == nil { // macOS 15+
		out, err = execOutput("/usr/bin/trash", "-v", p)
	} else if isDarwin() {
		script := "on run argv\ntell application \"Finder\" to delete (POSIX file (item 1 of argv) as alias)\nend run"
		out, err = execOutput("osascript", "-e", script, p)
	} else {
		out, err = execOutput("gio", "trash", p)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	// /usr/bin/trash -v prints: # Moved "<src>" to "<dst>"
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndex(line, `" to "`); i >= 0 && strings.HasSuffix(line, `"`) {
		return line[i+len(`" to "`) : len(line)-1], nil
	}
	return "", nil
}
