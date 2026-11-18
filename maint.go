package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Keeping a finished scan current without rescanning everything: rescan one
// folder, re-read free space, trash a whole selection.

// GET /api/volume → free/total for the scanned volume (cheap; statfs only).
func (a *App) handleVolume(w http.ResponseWriter, r *http.Request) {
	root := "/"
	if s := a.current(); s != nil {
		root = s.Root
	}
	writeJSON(w, volumeOf(root))
}

// POST /api/rescan {id} → walks that folder again and swaps the fresh subtree
// into the result. Nodes outside it keep their ids, so picks elsewhere survive.
func (a *App) handleRescan(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	var req struct {
		ID int `json:"id"`
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
	case !n.IsDir:
		httpErr(w, http.StatusBadRequest, "not a folder")
		return
	case n.Parent == nil:
		httpErr(w, http.StatusBadRequest, "use Scan for the whole folder")
		return
	}

	t0 := time.Now()
	s := newScan(p, 0)
	sub := &Node{Name: n.Name, IsDir: true, Mod: n.Mod}
	if fi, err := os.Stat(p); err == nil {
		sub.Mod = fi.ModTime().Unix()
	} else if errors.Is(err, os.ErrNotExist) {
		httpErr(w, http.StatusGone, "folder no longer exists")
		return
	}
	s.walk(sub, p, -1, liveCtx{self: s.live})
	s.wg.Wait()

	res.mu.Lock()
	if n.isGone() {
		res.mu.Unlock()
		httpErr(w, http.StatusConflict, "folder changed while rescanning")
		return
	}
	before := n.Size
	res.replace(n, sub)
	res.mu.Unlock()
	writeJSON(w, map[string]any{
		"id": sub.ID, "size": sub.Size, "files": sub.Files, "delta": sub.Size - before,
		"errors": s.errs.Load(), "ms": time.Since(t0).Milliseconds(),
	})
}

// replace swaps a freshly walked subtree in for old and fixes every aggregate:
// ancestor sizes, kind/extension totals, largest files and hints.
// Caller holds r.mu for writing.
func (r *Result) replace(old, sub *Node) {
	p := old.Parent
	sub.Parent = p
	inHint, inApp := false, false
	for a := p; a != nil; a = a.Parent {
		if strings.HasSuffix(a.Name, ".app") {
			inApp = true
		}
		if _, _, ok := dirHint(a); ok {
			inHint = true
		}
	}
	r.adjustStats(old, -1)
	b := &builder{r: r}
	b.visit(sub, inHint, inApp) // new ids, stats, hints; largest files collected in b.top

	for i, c := range p.Children {
		if c == old {
			p.Children[i] = sub
			break
		}
	}
	for a := p; a != nil; a = a.Parent {
		a.Size += sub.Size - old.Size
		a.Files += sub.Files - old.Files
	}
	sort.SliceStable(p.Children, func(i, j int) bool { return p.Children[i].Size > p.Children[j].Size })
	old.Gone = true

	top := make([]*Node, 0, len(r.Top)+len(b.top))
	for _, n := range r.Top {
		if !n.isGone() {
			top = append(top, n)
		}
	}
	top = append(top, b.top...)
	sort.Slice(top, func(i, j int) bool { return top[i].Size > top[j].Size })
	if len(top) > topFilesKeep {
		top = top[:topFilesKeep]
	}
	r.Top = top

	hints := make([]Hint, 0, len(r.Hints))
	for _, h := range r.Hints {
		if !h.Node.isGone() {
			hints = append(hints, h)
		}
	}
	sort.SliceStable(hints, func(i, j int) bool { return hints[i].Node.Size > hints[j].Node.Size })
	r.Hints = hints
}

// topPicks resolves picked ids to nodes, dropping anything whose ancestor is
// also picked (it goes with the parent). Biggest first. Caller holds r.mu.
func (r *Result) topPicks(ids []int) []*Node {
	picked := map[*Node]bool{}
	for _, id := range ids {
		if n := r.get(id); n != nil {
			picked[n] = true
		}
	}
	var items []*Node
	for n := range picked {
		covered := false
		for p := n.Parent; p != nil; p = p.Parent {
			if picked[p] {
				covered = true
				break
			}
		}
		if !covered {
			items = append(items, n)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Size > items[j].Size })
	return items
}

// POST /api/trash-batch {ids} → moves the picked items to the Trash in one go.
// Same rules as scripts: nested picks collapse, protected folders are left out.
func (a *App) handleTrashBatch(w http.ResponseWriter, r *http.Request) {
	res := a.result(w)
	if res == nil {
		return
	}
	var req struct {
		IDs []int `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "bad request")
		return
	}
	type item struct {
		n *Node
		p string
	}
	var todo []item
	skipped := 0
	res.mu.RLock()
	for _, n := range res.topPicks(req.IDs) {
		p := n.Path()
		if n.Parent == nil || protection(p) != protNone {
			skipped++
			continue
		}
		todo = append(todo, item{n, p})
	}
	res.mu.RUnlock()
	if len(todo) == 0 {
		httpErr(w, http.StatusBadRequest, "nothing to move")
		return
	}

	paths := make([]string, len(todo))
	for i, t := range todo {
		paths[i] = t.p
	}
	dests, trashErr := moveManyToTrash(paths)

	var moved, failed int
	var bytes int64
	undos := []string{}
	res.mu.Lock()
	for _, t := range todo {
		if _, err := os.Lstat(t.p); err == nil { // still there
			failed++
			continue
		}
		if !t.n.Gone {
			res.remove(t.n)
		}
		moved++
		bytes += t.n.Size
		if d := dests[t.p]; d != "" {
			undos = append(undos, rememberUndo(undoRec{res: res, node: t.n, orig: t.p, trashed: d, at: time.Now()}))
		}
	}
	res.mu.Unlock()
	out := map[string]any{"moved": moved, "bytes": bytes, "skipped": skipped, "failed": failed, "undo": undos}
	if trashErr != nil && failed > 0 {
		out["error"] = trashErr.Error()
	}
	writeJSON(w, out)
}

// moveManyToTrash trashes paths with one /usr/bin/trash call when available and
// returns where each one went.
func moveManyToTrash(paths []string) (map[string]string, error) {
	dests := map[string]string{}
	if _, err := os.Stat("/usr/bin/trash"); err != nil {
		var firstErr error
		for _, p := range paths {
			d, err := moveToTrash(p)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			dests[p] = d
		}
		return dests, firstErr
	}
	out, err := execOutput("/usr/bin/trash", append([]string{"-v"}, paths...)...)
	for _, line := range strings.Split(string(out), "\n") {
		// # Moved "<src>" to "<dst>"
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `# Moved "`) || !strings.HasSuffix(line, `"`) {
			continue
		}
		if i := strings.LastIndex(line, `" to "`); i > len(`# Moved "`) {
			dests[line[len(`# Moved "`):i]] = line[i+len(`" to "`) : len(line)-1]
		}
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return dests, errors.New(msg)
	}
	return dests, nil
}
