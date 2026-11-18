package main

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Node is one file or directory in the scanned tree.
type Node struct {
	Name     string
	Parent   *Node
	Children []*Node // nil for files
	Size     int64   // allocated bytes on disk (whole subtree for dirs)
	Files    int64   // number of files in subtree (1 for a file)
	Mod      int64   // unix seconds
	ID       int32
	IsDir    bool
	Err      bool // directory could not be read
	Skipped  bool // directory intentionally not scanned
	Gone     bool // moved to trash
}

func (n *Node) Path() string {
	if n.Parent == nil {
		return n.Name
	}
	return joinPath(n.Parent.Path(), n.Name)
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// isGone reports whether the node or any ancestor was trashed.
func (n *Node) isGone() bool {
	for p := n; p != nil; p = p.Parent {
		if p.Gone {
			return true
		}
	}
	return false
}

// Paths that are never descended into unless they are (or contain) the scan root.
// On macOS, /System/Volumes/Data is reachable again through firmlinks (/Users,
// /Applications, ...) so scanning it would double count. .nofollow/.resolve are
// magic aliases of the root, /Volumes holds other drives.
var skipPaths = map[string]bool{
	"/System/Volumes/Data": true,
	"/Volumes":             true,
	"/.nofollow":           true,
	"/.resolve":            true,
	"/.vol":                true,
	"/dev":                 true,
	"/proc":                true,
	"/sys":                 true,
	"/run":                 true,
}

type Scan struct {
	Root    string
	Gen     int
	Started time.Time

	finished  atomic.Int64 // unix nanos, 0 while running
	files     atomic.Int64
	dirs      atomic.Int64
	bytes     atomic.Int64
	errs      atomic.Int64
	current   atomic.Pointer[string]
	cancelled atomic.Bool
	result    atomic.Pointer[Result]
	live      *liveNode // running totals for the UI while walking

	stateMu    sync.Mutex
	state      string // scanning | done | cancelled | error
	fatal      string
	errSamples []string

	linkMu sync.Mutex
	links  map[[2]uint64]struct{}

	sem chan struct{}
	wg  sync.WaitGroup

	walkDur, buildDur time.Duration                                   // for -bench
	portable          bool                                            // force the os.ReadDir+lstat walker
	noLinkDedupe      bool                                            // -bench parity checks: hard-link attribution is order dependent
	slow              func(path string, entries int, d time.Duration) // -bench profiling hook
	errLog            func(path string, err error)                    // -bench: every error
}

func newScan(root string, gen int) *Scan {
	return &Scan{
		Root:    root,
		Gen:     gen,
		Started: time.Now(),
		state:   "scanning",
		links:   make(map[[2]uint64]struct{}),
		sem:     make(chan struct{}, scanWorkers()),
		live:    &liveNode{name: root, dir: true},
	}
}

// scanWorkers is how many directories are listed concurrently. Listing is
// bound by the kernel (APFS metadata and vnode locks): on a 10-core M-series
// Mac throughput stops improving at ~8 workers, while 40 workers burn twice
// the CPU in lock contention for the same wall time. One per core it is.
func scanWorkers() int {
	return max(4, runtime.NumCPU())
}

func (s *Scan) getState() (string, string, []string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state, s.fatal, append([]string(nil), s.errSamples...)
}

func (s *Scan) finish(state, fatal string) {
	s.stateMu.Lock()
	s.state, s.fatal = state, fatal
	s.stateMu.Unlock()
	s.finished.Store(time.Now().UnixNano())
}

func (s *Scan) elapsed() time.Duration {
	if f := s.finished.Load(); f != 0 {
		return time.Duration(f - s.Started.UnixNano())
	}
	return time.Since(s.Started)
}

func (s *Scan) recordErr(path string, err error) {
	if s.errLog != nil {
		s.errLog(path, err)
	}
	if s.errs.Add(1) <= 25 {
		s.stateMu.Lock()
		msg := path
		if pe, ok := err.(*os.PathError); ok {
			msg += " (" + pe.Err.Error() + ")"
		}
		s.errSamples = append(s.errSamples, msg)
		s.stateMu.Unlock()
	}
}

func (s *Scan) run() {
	info, err := os.Stat(s.Root)
	if err != nil {
		s.finish("error", err.Error())
		return
	}
	if !info.IsDir() {
		s.finish("error", s.Root+" is not a folder")
		return
	}
	root := &Node{Name: s.Root, IsDir: true, Mod: info.ModTime().Unix()}
	t0 := time.Now()
	s.walk(root, s.Root, -1, liveCtx{self: s.live})
	s.wg.Wait()
	s.walkDur = time.Since(t0)
	if s.cancelled.Load() {
		s.finish("cancelled", "")
		return
	}
	s.links = nil
	t1 := time.Now()
	res := buildResult(root)
	s.buildDur = time.Since(t1)
	s.result.Store(res)
	s.finish("done", "")
	debug.FreeOSMemory() // hand scan-time garbage back to the OS
}

func (s *Scan) skip(p string) bool {
	if !skipPaths[p] {
		return false
	}
	return !(s.Root == p || strings.HasPrefix(s.Root, p+"/"))
}

// dispatch walks a directory on a new goroutine if a worker slot is free,
// otherwise inline on the current one. It takes ownership of fd.
func (s *Scan) dispatch(n *Node, path string, fd int, lc liveCtx) {
	select {
	case s.sem <- struct{}{}:
		s.wg.Add(1)
		go func() {
			defer func() { <-s.sem; s.wg.Done() }()
			s.walk(n, path, fd, lc)
		}()
	default:
		s.walk(n, path, fd, lc)
	}
}

// rawEntry is one directory entry as read by a platform walker, before it
// becomes a Node. Names live in dirBuf.names to avoid per-entry allocations.
type rawEntry struct {
	nameOff, nameLen uint32
	dir              bool
	restat           bool // darwin mount point: real attributes come from the opened directory
	nlink            uint32
	size             int64 // allocated bytes on disk
	mod              int64 // unix seconds
	dev, ino         uint64
}

// dirBuf is per-worker scratch space, reused across directories.
type dirBuf struct {
	ents  []rawEntry
	names []byte
	io    []byte // raw getattrlistbulk output (darwin)
}

var dirBufPool = sync.Pool{New: func() any { return new(dirBuf) }}

func (b *dirBuf) add(name []byte, e rawEntry) {
	e.nameOff, e.nameLen = uint32(len(b.names)), uint32(len(name))
	b.names = append(b.names, name...)
	b.ents = append(b.ents, e)
}

// walk lists one directory and recurses into its subdirectories. fd is an open
// descriptor for the directory (owned and closed by walk), or -1 for the root.
func (s *Scan) walk(n *Node, path string, fd int, lc liveCtx) {
	if s.cancelled.Load() {
		if fd >= 0 {
			syscall.Close(fd)
		}
		return
	}
	if s.dirs.Add(1)&31 == 1 { // progress display only; skip most updates
		p := path
		s.current.Store(&p)
	}
	if fd < 0 { // the root: may be a symlink the user typed, so follow it
		var err error
		for {
			fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
			if err != syscall.EINTR {
				break
			}
		}
		if err != nil {
			n.Err = true
			s.recordErr(path, &os.PathError{Op: "open", Path: path, Err: err})
			return
		}
	}
	// Held while subdirectories are opened (and walked inline), so the kernel
	// resolves one name per openat instead of the whole path each time.
	defer syscall.Close(fd)

	b := dirBufPool.Get().(*dirBuf)
	b.ents, b.names = b.ents[:0], b.names[:0]
	var t0 time.Time
	if s.slow != nil {
		t0 = time.Now()
	}
	err := s.readDir(fd, path, b)
	if s.slow != nil {
		s.slow(path, len(b.ents), time.Since(t0))
	}
	if err != nil && len(b.ents) == 0 {
		dirBufPool.Put(b)
		n.Err = true
		s.recordErr(path, err)
		return
	}

	// One allocation each for all child nodes, their pointers and their names.
	k := len(b.ents)
	slab := make([]Node, k)
	kids := make([]*Node, k)
	arena := string(b.names)
	var bytes, files, dirOwn int64
	var restat []int         // rare: mount points
	var liveKids []*liveNode // only for the root and its children
	if lc.depth < 2 {
		liveKids = make([]*liveNode, k)
	}
	for i := range b.ents {
		e := &b.ents[i]
		c := &slab[i]
		c.Name = arena[e.nameOff : e.nameOff+e.nameLen]
		c.Parent = n
		c.Mod = e.mod
		c.Size = e.size
		kids[i] = c
		if e.dir {
			c.IsDir = true
			if e.restat {
				restat = append(restat, i)
			}
			dirOwn += c.Size
		} else {
			if e.nlink > 1 && !s.noLinkDedupe && s.seenLink(e.dev, e.ino) { // count hard-linked files once
				c.Size = 0
			}
			c.Files = 1
			bytes += c.Size
			files++
		}
		if liveKids != nil {
			ln := &liveNode{name: c.Name, dir: c.IsDir, mod: c.Mod}
			ln.bytes.Store(c.Size)
			ln.files.Store(c.Files)
			liveKids[i] = ln
		}
	}
	dirBufPool.Put(b)
	n.Children = kids
	s.files.Add(files)
	s.bytes.Add(bytes)
	lc.add(bytes+dirOwn, files)
	if liveKids != nil {
		lc.self.kids.Store(&liveKids)
	}

	for i, c := range kids {
		if !c.IsDir {
			continue
		}
		if s.cancelled.Load() {
			break
		}
		cp := joinPath(path, c.Name)
		if s.skip(cp) {
			c.Skipped = true
			continue
		}
		cfd, err := openDirAt(fd, c.Name, cp)
		if err != nil {
			s.dirs.Add(1)
			c.Err = true
			s.recordErr(cp, &os.PathError{Op: "open", Path: cp, Err: err})
			continue
		}
		if len(restat) > 0 && slices.Contains(restat, i) {
			if mod, size, ok := statDir(cfd); ok {
				c.Mod, c.Size = mod, size
			}
		}
		var ln *liveNode
		if liveKids != nil {
			ln = liveKids[i]
		}
		s.dispatch(c, cp, cfd, lc.child(ln))
	}
}

// seenLink reports whether this (dev, inode) was already counted.
func (s *Scan) seenLink(dev, ino uint64) bool {
	key := [2]uint64{dev, ino}
	s.linkMu.Lock()
	_, dup := s.links[key]
	if !dup {
		s.links[key] = struct{}{}
	}
	s.linkMu.Unlock()
	return dup
}

// readDirPortable lists a directory with os.ReadDir + lstat. Works everywhere.
func (s *Scan) readDirPortable(path string, b *dirBuf) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	for _, e := range entries {
		info, ierr := e.Info() // lstat: symlinks are never followed
		if ierr != nil {
			if !os.IsNotExist(ierr) {
				s.recordErr(joinPath(path, e.Name()), ierr)
			}
			continue
		}
		re := rawEntry{dir: e.IsDir(), size: info.Size(), mod: info.ModTime().Unix()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			re.size = int64(st.Blocks) * 512 // real space used on disk
			re.nlink = uint32(st.Nlink)
			re.dev, re.ino = uint64(st.Dev), uint64(st.Ino)
		}
		b.add([]byte(e.Name()), re)
	}
	return err
}

func expandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = home + p[1:]
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
