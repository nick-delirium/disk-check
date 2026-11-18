package main

import (
	"os"
	"strings"
	"testing"
)

func TestProtection(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]int{
		"/":                       protHard,
		"/Users":                  protHard,
		"/System":                 protHard,
		"/Applications":           protHard,
		home:                      protHard,
		home + "/Library":         protHard,
		home + "/Documents":       protSoft,
		home + "/Desktop":         protSoft,
		home + "/Downloads":       protSoft,
		home + "/Documents/x":     protNone,
		home + "/Library/Caches":  protNone,
		home + "/.npm":            protNone,
		"/Applications/Foo.app":   protNone,
		"/Users/someone-else/tmp": protNone,
	}
	for p, want := range cases {
		if got := protection(p); got != want {
			t.Errorf("protection(%q) = %d, want %d", p, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/a/b":           `'/a/b'`,
		"/it's here":     `'/it'\''s here'`,
		"/$(rm -rf ~)":   `'/$(rm -rf ~)'`,
		"/a b/`x`;# y":   "'/a b/`x`;# y'",
		"/new\nline":     "'/new\nline'",
		`/back\slash"q"`: `'/back\slash"q"'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBuildScriptSkipsNestedAndProtected(t *testing.T) {
	home, _ := os.UserHomeDir()
	// Tree: home → {Documents → {proj → {node_modules}}, Library}
	root := &Node{Name: home, IsDir: true}
	docs := &Node{Name: "Documents", IsDir: true, Parent: root}
	lib := &Node{Name: "Library", IsDir: true, Parent: root}
	proj := &Node{Name: "proj", IsDir: true, Parent: docs, Size: 100}
	nm := &Node{Name: "node_modules", IsDir: true, Parent: proj, Size: 60}
	root.Children = []*Node{docs, lib}
	docs.Children = []*Node{proj}
	proj.Children = []*Node{nm}
	res := buildResult(root)

	out := buildScript(res, scriptReq{IDs: []int{int(proj.ID), int(nm.ID), int(docs.ID), int(lib.ID)}, Mode: "rm"})
	// docs (soft) and lib (hard) are protected; nm is covered by proj... but proj is covered by docs,
	// so with docs picked, proj and nm collapse into docs, which is then left out as protected.
	if out.Count != 0 || out.Skipped != 2 {
		t.Fatalf("got count=%d skipped=%d script=%q", out.Count, out.Skipped, out.Script)
	}

	out = buildScript(res, scriptReq{IDs: []int{int(proj.ID), int(nm.ID)}, Mode: "rm"})
	want := "rm -rf -- " + shellQuote(home+"/Documents/proj") + "\n"
	if out.Count != 1 || out.Script != want {
		t.Fatalf("got count=%d script=%q, want %q", out.Count, out.Script, want)
	}
	if strings.Contains(out.Script, "#") {
		t.Fatalf("script must not contain comments (zsh pastes them as arguments): %q", out.Script)
	}
}
