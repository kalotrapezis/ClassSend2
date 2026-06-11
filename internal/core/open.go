package core

import (
	"os/exec"
	"runtime"
)

// openDefault opens a local file path or URL with the OS default handler.
// Used for teacher "push-open" (CmdPushOpen) and silent auto-open of received
// files. Cross-platform so student PCs get the same behaviour regardless of OS:
//
//   - Windows: cmd /c start "" <target>   (empty title arg is required so a
//     quoted path isn't mistaken for the window title)
//   - macOS:   open <target>
//   - Linux/*: xdg-open <target>          (xdg-utils; present on Mint, Fedora KDE)
//
// Fire-and-forget: the caller doesn't wait on the spawned viewer/browser.
func openDefault(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
