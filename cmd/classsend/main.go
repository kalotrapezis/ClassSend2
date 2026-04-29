package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"classsend/internal/core"
	"classsend/internal/ipc"
	"classsend/internal/monitoring"
	"classsend/internal/network"
	"classsend/internal/tui"
)

// defaultRole is overridden at build time via -ldflags "-X main.defaultRole=teacher"
// so we can ship teacher.exe and student.exe from the same source.
var defaultRole = "student"

func main() {
	role := flag.String("role", defaultRole, "Role: teacher or student")
	dev  := flag.Bool("dev", false, "Dev mode: scan localhost (same-machine testing)")
	flag.Parse()

	dataDir := dataDirectory()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("cannot create data directory: %v", err)
	}

	app, err := core.NewApp(core.Role(*role), dataDir, *dev)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	switch core.Role(*role) {
	case core.RoleTeacher:
		if err := app.StartTeacher(); err != nil {
			log.Fatalf("start teacher: %v", err)
		}

	case core.RoleStudent:
		// Try connecting to the background agent first.
		// If it's not running (e.g. dev mode, or agent not yet started),
		// fall back to a direct TCP connection.
		if conn, err := ipc.Dial(); err == nil {
			app.ConnectViaAgent(conn)
		} else {
			if err := app.StartStudent(); err != nil {
				log.Fatalf("start student: %v", err)
			}
		}

	default:
		log.Fatalf("unknown role: %s", *role)
	}

	model := tui.New(app)

	// Teacher: wire student join/leave sidebar events
	if core.Role(*role) == core.RoleTeacher {
		app.OnStudentJoin = func(st *network.Student) {
			model.PushStudentJoin(st.ID, st.Nickname, st.IP)
		}
		app.OnStudentLeave = func(st *network.Student) {
			model.PushStudentLeave(st.ID)
		}
		wireMonitoring(app)
		wireCasting(app)
	}

	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func dataDirectory() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData, _ = os.UserHomeDir()
	}
	return filepath.Join(appData, "ClassSend")
}

func wireMonitoring(app *core.App) {
	shotCh := make(chan monitoring.ShotMsg, 8)

	app.OnScreenshot = func(studentID string, jpegData []byte) {
		select {
		case shotCh <- monitoring.ShotMsg{StudentID: studentID, Data: jpegData}:
		default:
		}
	}

	var (
		sessionMu   sync.Mutex
		sessionStop func()
	)

	app.OnMonitoringStart = func() {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if sessionStop != nil {
			return
		}

		getStudents := func() []monitoring.StudentInfo {
			students := app.Server.Students()
			out := make([]monitoring.StudentInfo, len(students))
			for i, st := range students {
				out[i] = monitoring.StudentInfo{
					ID:       st.ID,
					Hostname: st.Hostname,
					Nickname: st.Nickname,
				}
			}
			return out
		}

		sendCmd := func(studentID string) error {
			return app.RequestShot(studentID)
		}

		stop, err := monitoring.StartSession(getStudents, sendCmd, shotCh, findMonitoringExe())
		if err != nil {
			log.Printf("monitoring: %v", err)
			return
		}
		sessionStop = stop
	}

	app.OnMonitoringStop = func() {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if sessionStop != nil {
			sessionStop()
			sessionStop = nil
		}
	}
}

func wireCasting(app *core.App) {
	var (
		castMu    sync.Mutex
		castSrv   *network.CastServer
		castStop  chan struct{}
	)

	app.OnStartCasting = func() (string, error) {
		castMu.Lock()
		defer castMu.Unlock()
		if castSrv != nil {
			return castSrv.LocalAddr(), nil // already running
		}
		srv, err := network.NewCastServer()
		if err != nil {
			return "", err
		}
		castSrv = srv

		ch := make(chan struct{})
		castStop = ch
		go runCastCapture(srv, ch)

		return srv.LocalAddr(), nil
	}

	app.OnStopCasting = func() {
		castMu.Lock()
		defer castMu.Unlock()
		if castStop != nil {
			close(castStop)
			castStop = nil
		}
		if castSrv != nil {
			castSrv.Close()
			castSrv = nil
		}
	}
}

func findMonitoringExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "monitoring.exe"
	}
	return filepath.Join(filepath.Dir(exe), "monitoring.exe")
}
