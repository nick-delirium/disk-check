package main

import (
	"os"
	"path/filepath"
	"testing"
)

func scanDir(t *testing.T, dir string) *Result {
	t.Helper()
	s := newScan(dir, 1)
	s.run()
	res := s.result.Load()
	if res == nil {
		st, fatal, _ := s.getState()
		t.Fatalf("scan failed: %s %s", st, fatal)
	}
	return res
}

func TestRescanReplacesSubtree(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "keep"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "a", "b", "old.bin"), make([]byte, 64<<10), 0o644))
	must(os.WriteFile(filepath.Join(dir, "keep", "k.bin"), make([]byte, 32<<10), 0o644))

	res := scanDir(t, dir)
	a := res.findPath(filepath.Join(dir, "a"))
	keep := res.findPath(filepath.Join(dir, "keep"))
	if a == nil || keep == nil {
		t.Fatal("folders not found")
	}
	keepID, oldAID, rootBefore := keep.ID, a.ID, res.Root.Size

	// Change the disk under "a": drop old.bin, add a bigger file.
	must(os.Remove(filepath.Join(dir, "a", "b", "old.bin")))
	must(os.WriteFile(filepath.Join(dir, "a", "new.dat"), make([]byte, 512<<10), 0o644))

	s := newScan(filepath.Join(dir, "a"), 0)
	sub := &Node{Name: "a", IsDir: true}
	s.walk(sub, filepath.Join(dir, "a"), -1, liveCtx{self: s.live})
	s.wg.Wait()
	before := a.Size
	res.replace(a, sub)

	if res.get(int(oldAID)) != nil {
		t.Error("old folder id should be gone")
	}
	if got := res.get(int(sub.ID)); got != sub {
		t.Error("new folder id should resolve to the new subtree")
	}
	if res.get(int(keepID)) != keep {
		t.Error("ids outside the rescanned folder must stay stable")
	}
	if want := rootBefore + sub.Size - before; res.Root.Size != want {
		t.Errorf("root size %d, want %d", res.Root.Size, want)
	}
	if res.Root.Files != 2 {
		t.Errorf("root files %d, want 2", res.Root.Files)
	}
	if len(res.Top) == 0 || res.Top[0].Name != "new.dat" {
		t.Errorf("largest file should be new.dat, got %+v", res.Top)
	}
	for _, n := range res.Top {
		if n.Name == "old.bin" {
			t.Error("deleted file still listed in largest files")
		}
	}
	if st := res.Exts[".dat"]; st == nil || st.Count != 1 {
		t.Errorf(".dat stats not added: %+v", st)
	}
	if st := res.Exts[".bin"]; st == nil || st.Count != 1 {
		t.Errorf(".bin should only count keep/k.bin now: %+v", st)
	}
}
