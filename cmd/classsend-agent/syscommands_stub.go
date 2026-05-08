//go:build !windows

package main

import "classsend/internal/core"

func setupStudentCommands(_ *core.App, _ bool) {}
func startHealthBeacon()                        {}
func hideConsole()                              {}
func ensureAutostart()                          {}
func setAutostart(_ bool) error                 { return nil }
func isAutostartEnabled() bool                  { return false }
