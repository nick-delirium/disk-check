package main

import (
	"sort"
	"sync/atomic"
)

// Live view of a scan in progress: running totals for the scan root's children
// and grandchildren, so the UI can draw a treemap that fills in while walking.
// Walkers add to these counters once per directory (not per file), and child
// lists are published once, after they are complete, so readers never race.

type liveNode struct {
	name  string
	dir   bool
	mod   int64
	bytes atomic.Int64
	files atomic.Int64
	kids  atomic.Pointer[[]*liveNode]
}

// liveCtx tells a walker which live counters its directory feeds.
// depth 0 is the scan root, 1 its children, 2 its grandchildren.
type liveCtx struct {
	depth  int
	self   *liveNode // this directory's own live node (depth <= 2), else nil
	l1, l2 *liveNode // ancestors at depth 1 and 2 (or self), nil above them
}

// child returns the context for a subdirectory, given its live node (if any).
func (lc liveCtx) child(n *liveNode) liveCtx {
	c := liveCtx{depth: lc.depth + 1, l1: lc.l1, l2: lc.l2}
	switch c.depth {
	case 1:
		c.l1, c.self = n, n
	case 2:
		c.l2, c.self = n, n
	}
	return c
}

// add credits bytes/files found directly in this directory to its ancestors.
func (lc liveCtx) add(bytes, files int64) {
	if lc.l1 != nil {
		lc.l1.bytes.Add(bytes)
		lc.l1.files.Add(files)
	}
	if lc.l2 != nil {
		lc.l2.bytes.Add(bytes)
		lc.l2.files.Add(files)
	}
}

type liveOut struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Files int64     `json:"files"`
	Mod   int64     `json:"mod,omitempty"`
	Dir   bool      `json:"dir"`
	Kids  []liveOut `json:"kids,omitempty"`
}

func liveList(p *[]*liveNode, limit int) (out []liveOut, more int, moreSize int64) {
	if p == nil {
		return nil, 0, 0
	}
	all := make([]liveOut, 0, len(*p))
	for _, n := range *p {
		all = append(all, liveOut{Name: n.name, Size: n.bytes.Load(), Files: n.files.Load(), Mod: n.mod, Dir: n.dir})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Size > all[j].Size })
	if len(all) > limit {
		for _, o := range all[limit:] {
			more++
			moreSize += o.Size
		}
		all = all[:limit]
	}
	return all, more, moreSize
}

// liveSnapshot returns the root's children (top 300) with their top 40 children.
func (s *Scan) liveSnapshot() map[string]any {
	root := s.live
	kids := root.kids.Load()
	out, more, moreSize := liveList(kids, 300)
	if kids != nil {
		byName := make(map[string]*liveNode, len(*kids))
		for _, n := range *kids {
			byName[n.name] = n
		}
		for i := range out {
			if n := byName[out[i].Name]; n != nil && n.dir && i < 250 {
				out[i].Kids, _, _ = liveList(n.kids.Load(), 40)
			}
		}
	}
	if out == nil {
		out = []liveOut{}
	}
	return map[string]any{
		"size": s.bytes.Load(), "files": s.files.Load(),
		"children": out, "more": more, "moreSize": moreSize,
	}
}
