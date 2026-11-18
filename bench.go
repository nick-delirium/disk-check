package main

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// runBench scans one folder synchronously and prints timings plus a digest of
// the resulting tree, so scanner changes can be measured and checked for parity.
//
//	DC_WORKERS=n   override the number of concurrent directory workers
//	DC_WALKER=portable  force the portable os.ReadDir+lstat walker
//	DC_DUMP=file   also write one "path size dir mod err" line per node
//	DC_NOLINKS=1   don't dedupe hard links (makes per-file sizes order independent)
//	DC_BULKBUF=n   getattrlistbulk buffer size in bytes (darwin)
func runBench(path string) {
	p, err := expandPath(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s := newScan(p, 1)
	if n, _ := strconv.Atoi(os.Getenv("DC_WORKERS")); n > 0 {
		s.sem = make(chan struct{}, n)
	}
	s.portable = os.Getenv("DC_WALKER") == "portable"
	s.noLinkDedupe = os.Getenv("DC_NOLINKS") != ""
	setBenchTuning()
	type slowDir struct {
		path string
		n    int
		d    time.Duration
	}
	var slowMu sync.Mutex
	var slows []slowDir
	if os.Getenv("DC_SLOW") != "" {
		s.slow = func(path string, n int, d time.Duration) {
			if d > 20*time.Millisecond {
				slowMu.Lock()
				slows = append(slows, slowDir{path, n, d})
				slowMu.Unlock()
			}
		}
	}
	var allErrs []string
	if os.Getenv("DC_ERRS") != "" {
		s.errLog = func(path string, err error) {
			slowMu.Lock()
			allErrs = append(allErrs, path+" | "+err.Error())
			slowMu.Unlock()
		}
	}
	s.run()
	if len(slows) > 0 {
		sort.Slice(slows, func(i, j int) bool { return slows[i].d > slows[j].d })
		var sum time.Duration
		for _, x := range slows {
			sum += x.d
		}
		fmt.Printf("%d dirs took >20ms to list (sum %v):\n", len(slows), sum.Round(time.Millisecond))
		for _, x := range slows[:min(15, len(slows))] {
			fmt.Printf("  %6dms %7d entries  %s\n", x.d.Milliseconds(), x.n, x.path)
		}
	}
	if len(allErrs) > 0 {
		sort.Strings(allErrs)
		for _, e := range allErrs {
			fmt.Println("  err:", e)
		}
	}
	state, fatal, _ := s.getState()
	if state != "done" {
		fmt.Fprintln(os.Stderr, state, fatal)
		os.Exit(1)
	}
	r := s.result.Load()

	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	maxrss := int64(ru.Maxrss)
	if runtime.GOOS == "linux" {
		maxrss *= 1024
	}
	ms := func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + "ms" }
	fmt.Printf("walker=%s workers=%d walk=%s build=%s total=%s files=%d dirs=%d bytes=%d errors=%d maxrss=%dMB digest=%x\n",
		s.walkerName(), cap(s.sem), ms(s.walkDur), ms(s.buildDur), ms(s.walkDur+s.buildDur),
		r.Root.Files, s.dirs.Load(), r.Root.Size, s.errs.Load(), maxrss>>20, digest(r.Root, os.Getenv("DC_DUMP")))
}

// digest hashes the tree in name order, independent of scan or sort order.
func digest(root *Node, dumpPath string) uint64 {
	h := fnv.New64a()
	var w *bufio.Writer
	if dumpPath != "" {
		f, err := os.Create(dumpPath)
		if err == nil {
			defer f.Close()
			w = bufio.NewWriterSize(f, 1<<20)
			defer w.Flush()
		}
	}
	var visit func(n *Node, path string)
	visit = func(n *Node, path string) {
		line := fmt.Sprintf("%s\t%d\t%t\t%d\t%t\t%t\n", path, n.Size, n.IsDir, n.Mod, n.Err, n.Skipped)
		h.Write([]byte(line))
		if w != nil {
			w.WriteString(line)
		}
		kids := append([]*Node(nil), n.Children...)
		sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
		for _, c := range kids {
			visit(c, path+"/"+c.Name)
		}
	}
	visit(root, ".")
	return h.Sum64()
}
