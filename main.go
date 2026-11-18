package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	port := flag.Int("port", 0, "port to listen on (0 = pick a free one)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser automatically")
	window := flag.Bool("window", inAppBundle(), "show the UI in a native window instead of the browser (macOS)")
	bench := flag.String("bench", "", "scan `folder` once, print timings and exit (no UI)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: disk-check [flags] [folder]\n\nScans folder (default: your home folder) and opens a local web UI.\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *bench != "" {
		runBench(*bench)
		return
	}

	start := flag.Arg(0)
	if start == "" {
		start, _ = os.UserHomeDir()
	}

	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	app := &App{token: hex.EncodeToString(tok), port: ln.Addr().(*net.TCPAddr).Port}
	if start != "" {
		if _, err := app.startScan(start); err != nil {
			log.Printf("can't scan %s: %v", start, err)
		}
	}

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	app.routes(mux)

	url := fmt.Sprintf("http://127.0.0.1:%d/#t=%s", app.port, app.token)
	srv := &http.Server{Handler: app.guard(mux), ReadHeaderTimeout: 10 * time.Second}

	if *window && haveWindow {
		go func() { log.Fatal(srv.Serve(ln)) }()
		runWindow(url) // blocks until the window is closed / app quits
		return
	}

	fmt.Printf("\n  Disk Check is running → %s\n\n  Press Ctrl+C to quit.\n\n", url)
	if !*noBrowser {
		openBrowser(url)
	}
	log.Fatal(srv.Serve(ln))
}

// inAppBundle reports whether we were launched from inside a macOS .app bundle.
func inAppBundle() bool {
	exe, err := os.Executable()
	return err == nil && strings.Contains(exe, ".app/Contents/MacOS/")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
