package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// UI preferences that must survive restarts. They live on disk, not in the
// page: the server picks a new port each launch, so browser storage (scoped to
// origin = host:port) would start empty every time.

var prefKeys = map[string]bool{ // allowlist: the page can't write arbitrary data
	"fdaDismissed": true,
}

var prefsMu sync.Mutex

func prefsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "disk-check", "prefs.json")
}

func loadPrefs() map[string]any {
	prefs := map[string]any{}
	if p := prefsPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &prefs)
		}
	}
	return prefs
}

// GET /api/prefs → saved preferences.
func (a *App) handlePrefsGet(w http.ResponseWriter, r *http.Request) {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	writeJSON(w, loadPrefs())
}

// POST /api/prefs {key: value} → merges allowed keys and saves.
func (a *App) handlePrefsSet(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		httpErr(w, http.StatusBadRequest, "bad request")
		return
	}
	prefsMu.Lock()
	defer prefsMu.Unlock()
	prefs := loadPrefs()
	for k, v := range in {
		if prefKeys[k] {
			prefs[k] = v
		}
	}
	p := prefsPath()
	if p == "" {
		httpErr(w, http.StatusInternalServerError, "no config directory")
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, _ := json.MarshalIndent(prefs, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, prefs)
}
