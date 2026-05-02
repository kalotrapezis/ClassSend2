// Package devlog writes a per-session log file. The log lives next to the
// running .exe in a `logs\` subfolder by default (same directory as the
// program — easiest for the user to find), with fallbacks to %APPDATA%,
// %LOCALAPPDATA%, and %TEMP% if the exe directory isn't writable.
//
// Each binary (teacher, student, agent, monitoring) calls Init once at
// startup with its component name; from then on Logf() appends timestamped
// lines.  Files are named  <component>-YYYYMMDD-HHMMSS-<pid>.log .
//
// To disable in production builds, link with:
//
//	-ldflags="-X classsend/internal/devlog.disabled=1"
//
// When disabled all calls are no-ops and no file is created.
package devlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// disabled is set via -ldflags "-X classsend/internal/devlog.disabled=1" to
// turn logging off in shippable builds. Empty string = enabled.
var disabled = ""

var (
	mu        sync.Mutex
	file      *os.File
	component string
	started   time.Time
)

// Init opens the per-session log file. Safe to call multiple times — only the
// first call has effect. Tries several writable locations in order; if every
// one fails it drops a tiny diag-marker file next to the exe so the user can
// see *something* went wrong.
func Init(componentName string) {
	if disabled != "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		return
	}

	component = componentName
	started = time.Now()
	name := fmt.Sprintf("%s-%s-%d.log",
		componentName,
		started.Format("20060102-150405"),
		os.Getpid())

	var lastErr error
	for _, dir := range candidateDirs() {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			lastErr = err
			continue
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			lastErr = err
			continue
		}
		file = f
		writeRaw(fmt.Sprintf("=== %s session start  pid=%d  log=%s ===\n",
			componentName, os.Getpid(), f.Name()))
		return
	}

	// Every candidate failed. Drop a marker so the user can see this.
	dropFailureMarker(componentName, lastErr)
}

// Logf writes one timestamped line. Does nothing if logging is disabled or
// Init was never called / failed.
func Logf(format string, args ...any) {
	if disabled != "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return
	}
	ts := time.Since(started).Truncate(time.Millisecond)
	writeRaw(fmt.Sprintf("[%9s] %s\n", ts, fmt.Sprintf(format, args...)))
}

// Close flushes and closes the log file. Safe to call multiple times.
func Close() {
	if disabled != "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		writeRaw("=== session end ===\n")
		_ = file.Close()
		file = nil
	}
}

// Path returns the absolute path of the current session log, or "" if logging
// is disabled / inactive.
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return ""
	}
	return file.Name()
}

func writeRaw(line string) {
	if file == nil {
		return
	}
	_, _ = file.WriteString(line)
	_ = file.Sync()
}

// candidateDirs returns log-directory candidates in priority order. The exe
// directory is first because the user can find it immediately — same place
// as the program they just launched.
func candidateDirs() []string {
	var dirs []string

	// 1. logs/ next to the running .exe
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "logs"))
	}
	// 2. %APPDATA%\ClassSend\logs
	if v := os.Getenv("APPDATA"); v != "" {
		dirs = append(dirs, filepath.Join(v, "ClassSend", "logs"))
	}
	// 3. %LOCALAPPDATA%\ClassSend\logs
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		dirs = append(dirs, filepath.Join(v, "ClassSend", "logs"))
	}
	// 4. %TEMP%\ClassSend\logs
	dirs = append(dirs, filepath.Join(os.TempDir(), "ClassSend", "logs"))

	return dirs
}

// dropFailureMarker writes a tiny file recording why log init failed, in
// whatever location accepts a write. Without this, a silent Init failure
// is indistinguishable from "the new build wasn't installed".
func dropFailureMarker(componentName string, lastErr error) {
	body := fmt.Sprintf(
		"devlog.Init failed for %s at %s\nlast error: %v\nattempted dirs:\n",
		componentName, time.Now().Format(time.RFC3339), lastErr)
	for _, d := range candidateDirs() {
		body += "  " + d + "\n"
	}

	markerName := fmt.Sprintf("DEVLOG-FAILED-%s-%d.txt", componentName, os.Getpid())
	for _, base := range []string{".", os.TempDir()} {
		p := filepath.Join(base, markerName)
		if f, err := os.Create(p); err == nil {
			_, _ = f.WriteString(body)
			_ = f.Close()
			return
		}
	}
}
