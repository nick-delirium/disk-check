package main

import (
	"container/heap"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	topFilesKeep = 300
	minHintSize  = 10_000_000 // 10 MB
)

type Stat struct {
	Size  int64 `json:"size"`
	Count int64 `json:"count"`
}

type Hint struct {
	Node  *Node
	Label string
	Desc  string
}

// Result is the finished, queryable scan. mu guards mutations from trashing.
type Result struct {
	mu    sync.RWMutex
	Root  *Node
	Nodes []*Node
	Top   []*Node // largest files, biggest first
	Cats  map[string]*Stat
	Exts  map[string]*Stat
	Hints []Hint
}

func (r *Result) get(id int) *Node {
	if id < 0 || id >= len(r.Nodes) {
		return nil
	}
	n := r.Nodes[id]
	if n.isGone() {
		return nil
	}
	return n
}

// ---- building ----

type builder struct {
	r   *Result
	top nodeHeap
}

func buildResult(root *Node) *Result {
	r := &Result{Root: root, Cats: map[string]*Stat{}, Exts: map[string]*Stat{}}
	b := &builder{r: r}
	b.visit(root, false, false)
	sort.Slice(b.top, func(i, j int) bool { return b.top[i].Size > b.top[j].Size })
	r.Top = b.top
	sort.Slice(r.Hints, func(i, j int) bool { return r.Hints[i].Node.Size > r.Hints[j].Node.Size })
	return r
}

func (b *builder) visit(n *Node, inHint, inApp bool) {
	n.ID = int32(len(b.r.Nodes))
	b.r.Nodes = append(b.r.Nodes, n)

	if !n.IsDir {
		ext := extOf(n.Name)
		cat := categoryOf(ext, inApp)
		addStat(b.r.Cats, cat, n.Size, 1)
		addStat(b.r.Exts, extKey(ext), n.Size, 1)
		if len(b.top) < topFilesKeep {
			heap.Push(&b.top, n)
		} else if n.Size > b.top[0].Size {
			b.top[0] = n
			heap.Fix(&b.top, 0)
		}
		if !inHint && n.Size >= 100_000_000 {
			if label, desc, ok := fileHint(ext); ok {
				b.r.Hints = append(b.r.Hints, Hint{n, label, desc})
			}
		}
		return
	}

	var hint *Hint
	if !inHint {
		if label, desc, ok := dirHint(n); ok {
			hint = &Hint{n, label, desc}
		}
	}
	childApp := inApp || strings.HasSuffix(n.Name, ".app")
	for _, c := range n.Children {
		b.visit(c, inHint || hint != nil, childApp)
		n.Size += c.Size
		n.Files += c.Files
	}
	sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Size > n.Children[j].Size })
	if hint != nil && n.Size >= minHintSize {
		b.r.Hints = append(b.r.Hints, *hint)
	}
}

func addStat(m map[string]*Stat, key string, size, count int64) {
	s := m[key]
	if s == nil {
		s = &Stat{}
		m[key] = s
	}
	s.Size += size
	s.Count += count
}

// remove detaches a trashed node and fixes up every aggregate it was part of.
// Caller holds r.mu for writing.
func (r *Result) remove(n *Node) {
	p := n.Parent
	if p == nil {
		return
	}
	for i, c := range p.Children {
		if c == n {
			p.Children = append(p.Children[:i], p.Children[i+1:]...)
			break
		}
	}
	for a := p; a != nil; a = a.Parent {
		a.Size -= n.Size
		a.Files -= n.Files
	}
	r.adjustStats(n, -1)
	n.Gone = true
}

// adjustStats adds (sign 1) or removes (sign -1) a subtree's files from the kind/extension totals.
func (r *Result) adjustStats(n *Node, sign int64) {
	inApp := false
	for a := n.Parent; a != nil; a = a.Parent {
		if strings.HasSuffix(a.Name, ".app") {
			inApp = true
		}
	}
	var walk func(x *Node, app bool)
	walk = func(x *Node, app bool) {
		if !x.IsDir {
			ext := extOf(x.Name)
			addStat(r.Cats, categoryOf(ext, app), sign*x.Size, sign)
			addStat(r.Exts, extKey(ext), sign*x.Size, sign)
			return
		}
		app = app || strings.HasSuffix(x.Name, ".app")
		for _, c := range x.Children {
			walk(c, app)
		}
	}
	walk(n, inApp)
}

// hintKind marks hints whose folder is better handled by a command ("cmd") or that may hold
// things worth keeping ("review", left out of the reclaimable total).
var hintKind = map[string]string{
	"iOS Simulators": "cmd", "Docker Desktop": "cmd", "Go module cache": "cmd", "Ollama models": "cmd", "pnpm store": "cmd",
	"Xcode Archives": "review", "iPhone / iPad backups": "review", "Hugging Face models": "review", "Trash": "review",
}

// ---- min-heap for largest files ----

type nodeHeap []*Node

func (h nodeHeap) Len() int           { return len(h) }
func (h nodeHeap) Less(i, j int) bool { return h[i].Size < h[j].Size }
func (h nodeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x any)        { *h = append(*h, x.(*Node)) }
func (h *nodeHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// ---- file categories ----

func extOf(name string) string {
	e := filepath.Ext(name)
	if e == "" || e == name { // dotfiles like ".zshrc"
		return ""
	}
	return strings.ToLower(e[1:])
}

func extKey(ext string) string {
	if ext == "" {
		return "(none)"
	}
	return "." + ext
}

var extCategory = map[string]string{}

func init() {
	groups := map[string]string{
		"Video":            "mp4 mov m4v mkv avi webm wmv flv mpg mpeg 3gp mts m2ts prproj fcpbundle braw r3d",
		"Audio":            "mp3 m4a aac wav flac aiff aif ogg opus wma alac caf logicx band mid midi",
		"Images":           "jpg jpeg png gif heic heif webp tiff tif bmp raw cr2 cr3 nef arw dng orf rw2 psd psb ai svg ico icns xcf sketch fig afphoto afdesign exr hdr",
		"Documents":        "pdf doc docx xls xlsx ppt pptx key pages numbers txt md rtf csv tsv epub mobi odt ods odp tex log",
		"Archives":         "zip rar 7z tar gz tgz bz2 xz zst lz4 lzma br cpio jar war",
		"Disk images":      "dmg iso img pkg ipsw xip vmdk vdi qcow2 vhd vhdx sparseimage sparsebundle asif",
		"Code":             "js mjs cjs ts tsx jsx go rs py rb java kt kts swift c h cc cpp hpp m mm cs php lua sh zsh bash pl r scala dart ex exs erl hs ml zig nim vue svelte css scss sass less html htm json yaml yml toml xml sql proto graphql map lock",
		"Apps & libraries": "app dylib so a o framework bundle exe dll wasm node class pyc pyo whl gem car nib storyboardc",
		"Data":             "db sqlite sqlite3 realm parquet arrow npy npz h5 hdf5 pt pth ckpt safetensors gguf onnx onnx_data pb tflite bin dat pack idx",
	}
	for cat, list := range groups {
		for _, e := range strings.Fields(list) {
			extCategory[e] = cat
		}
	}
}

func categoryOf(ext string, inApp bool) string {
	if inApp {
		return "Apps & libraries"
	}
	if c, ok := extCategory[ext]; ok {
		return c
	}
	return "Other"
}

// ---- cleanup hints ----

func hasChild(n *Node, name string) bool {
	if n == nil {
		return false
	}
	for _, c := range n.Children {
		if c.Name == name {
			return true
		}
	}
	return false
}

func dirHint(n *Node) (label, desc string, ok bool) {
	name := n.Name
	parent, grand := "", ""
	if n.Parent != nil {
		parent = n.Parent.Name
		if n.Parent.Parent != nil {
			grand = n.Parent.Parent.Name
		}
	}
	switch {
	case name == "node_modules":
		return "node_modules", "Reinstall with `npm i`", true
	case name == "DerivedData" && parent == "Xcode":
		return "Xcode DerivedData", "Xcode rebuilds it", true
	case name == "Archives" && parent == "Xcode":
		return "Xcode Archives", "Keep builds you still symbolicate", true
	case strings.HasSuffix(name, "DeviceSupport") && parent == "Xcode":
		return "Xcode device support", "Re-downloaded when needed", true
	case name == "CoreSimulator" && parent == "Developer":
		return "iOS Simulators", "`xcrun simctl delete unavailable`", true
	case name == "Caches" && parent == "Library":
		return "App caches", "Recreated by apps; quit them first", true
	case name == "Logs" && parent == "Library":
		return "Logs", "Safe to delete", true
	case name == ".Trash":
		return "Trash", "Empty the Trash to free it", true
	case name == "Backup" && parent == "MobileSync":
		return "iPhone / iPad backups", "Manage in Finder › device", true
	case name == "target" && hasChild(n.Parent, "Cargo.toml"):
		return "Rust build output", "`cargo clean`", true
	case (name == ".venv" || name == "venv" || name == "env") && hasChild(n, "pyvenv.cfg"):
		return "Python virtualenv", "Recreate from requirements", true
	case name == ".gradle" && grand != "":
		return "Gradle cache", "Re-downloaded on next build", true
	case name == "repository" && parent == ".m2":
		return "Maven repository", "Re-downloaded on demand", true
	case name == "mod" && parent == "pkg" && grand == "go":
		return "Go module cache", "`go clean -modcache`", true
	case name == ".npm":
		return "npm cache", "`npm cache clean --force`", true
	case name == ".pnpm-store" || (name == "store" && parent == "pnpm"):
		return "pnpm store", "`pnpm store prune`", true
	case name == ".yarn" && hasChild(n, "berry"), name == "Yarn" && parent == "Caches":
		return "Yarn cache", "`yarn cache clean`", true
	case name == "huggingface" && parent == ".cache":
		return "Hugging Face models", "Downloaded weights and datasets", true
	case name == "models" && parent == ".ollama":
		return "Ollama models", "`ollama rm <model>`", true
	case name == "com.docker.docker" && parent == "Containers":
		return "Docker Desktop", "`docker system prune -a`", true
	case name == ".next" && hasChild(n.Parent, "package.json"):
		return "Next.js build", "Rebuilt by `next build`", true
	}
	return "", "", false
}

func fileHint(ext string) (label, desc string, ok bool) {
	switch ext {
	case "dmg", "pkg", "iso", "xip":
		return "Installer / disk image", "Not needed once installed", true
	case "ipsw":
		return "iOS firmware", "Re-downloaded when needed", true
	}
	return "", "", false
}
