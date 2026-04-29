//go:build !windows

package main

import "classsend/internal/network"

// Student system commands (lock, mute, screenshot, autostart) live in
// cmd/classsend-agent. This file satisfies any remaining references in main.go
// on all platforms.

func runCastCapture(_ *network.CastServer, _ <-chan struct{}) {}
