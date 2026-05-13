//go:build windows

package main

import (
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"classsend/internal/devlog"
)

// System-load monitor. Goal: when the student's PC is too overloaded for a
// screenshot to come back in any reasonable time (or to come back without
// kicking the agent off the GPU and crashing it), we skip the capture and
// return a "SYSTEM UNDER LOAD" placeholder via the same path the existing
// TECHNICAL DIFFICULTIES placeholder uses. The teacher still sees a clearly-
// marked cell, the agent doesn't compete with whatever's hammering the box,
// and recovery is automatic when load drops.
//
// Thresholds are deliberately conservative because the deployment target is
// "potato" classroom PCs that idle at 50–70% CPU under normal load — a 70%
// threshold would trigger the placeholder permanently. We only flip to
// CRITICAL on sustained near-peg CPU OR very-low free memory.

var (
	procGetSystemTimes        = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusExL = kernel32.NewProc("GlobalMemoryStatusEx")

	loadIsCritical atomic.Bool

	// Last sampled values, exposed for log lines on transitions.
	lastCPUPct atomic.Int32 // 0..100
	lastMemMB  atomic.Int64 // available physical MB
)

type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

// readSystemTimes returns (idle, kernel, user) as 100-ns ticks since boot.
// On error returns zeros — caller treats as "no sample this tick".
func readSystemTimes() (idle, kernel, user uint64) {
	var idleFT, kernelFT, userFT syscall.Filetime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	)
	if r == 0 {
		return 0, 0, 0
	}
	ftToU64 := func(ft syscall.Filetime) uint64 {
		return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
	}
	return ftToU64(idleFT), ftToU64(kernelFT), ftToU64(userFT)
}

// readAvailMB returns physical memory available to the agent's user-mode in MB.
// On error returns -1 (caller skips the memory check).
func readAvailMB() int64 {
	m := memoryStatusEx{}
	m.dwLength = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusExL.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return -1
	}
	return int64(m.ullAvailPhys / (1024 * 1024))
}

// isUnderLoad is a hot-path check; safe to call from any goroutine.
func isUnderLoad() bool { return loadIsCritical.Load() }

// startLoadMonitor kicks off a background goroutine that samples system CPU
// and available memory every 2 s and flips loadIsCritical on/off with
// hysteresis. Transitions are logged; steady state is silent.
func startLoadMonitor() {
	// Thresholds — tuned for "potato classroom PC" baseline. Numbers are
	// intentionally permissive: we'd rather let a slow PC limp along sending
	// real shots than mark it CRITICAL on every busy moment.
	const (
		tickSec          = 2
		cpuEnterPct      = 98 // CPU% to ENTER critical
		cpuEnterTicks    = 3  // ≥ 6 s of near-pegged CPU
		cpuExitPct       = 85 // CPU% to LEAVE critical
		cpuExitTicks     = 3  // ≥ 6 s of clear breathing room
		memEnterMB       = 150
		memEnterTicks    = 2
		memExitMB        = 300
		memExitTicks     = 2
	)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				devlog.Logf("load-monitor: panic: %v — monitor stopped", r)
			}
		}()

		// Prime the CPU sample so the first tick has a valid delta.
		prevIdle, prevKernel, prevUser := readSystemTimes()
		cpuHotTicks := 0
		cpuCoolTicks := 0
		memLowTicks := 0
		memOkTicks := 0

		for {
			time.Sleep(tickSec * time.Second)

			// CPU% from kernel-reported counters. Delta = total - idle.
			idle, kernel, user := readSystemTimes()
			cpuPct := -1
			if idle >= prevIdle && kernel >= prevKernel && user >= prevUser {
				dIdle := idle - prevIdle
				dKernel := kernel - prevKernel
				dUser := user - prevUser
				total := dKernel + dUser // dKernel already includes idle on Windows
				if total > 0 {
					busy := total - dIdle
					cpuPct = int(busy * 100 / total)
					if cpuPct < 0 {
						cpuPct = 0
					}
					if cpuPct > 100 {
						cpuPct = 100
					}
					lastCPUPct.Store(int32(cpuPct))
				}
			}
			prevIdle, prevKernel, prevUser = idle, kernel, user

			availMB := readAvailMB()
			if availMB >= 0 {
				lastMemMB.Store(availMB)
			}

			// Update CPU streak counters.
			if cpuPct >= cpuEnterPct {
				cpuHotTicks++
				cpuCoolTicks = 0
			} else if cpuPct >= 0 && cpuPct <= cpuExitPct {
				cpuCoolTicks++
				cpuHotTicks = 0
			} else if cpuPct >= 0 {
				// In the gap between cool and hot — stop accumulating either way.
				cpuHotTicks = 0
				cpuCoolTicks = 0
			}

			// Memory streak counters.
			if availMB >= 0 && availMB <= memEnterMB {
				memLowTicks++
				memOkTicks = 0
			} else if availMB >= memExitMB {
				memOkTicks++
				memLowTicks = 0
			}

			wasCritical := loadIsCritical.Load()
			enterCPU := cpuHotTicks >= cpuEnterTicks
			enterMem := memLowTicks >= memEnterTicks
			exitCPU := cpuCoolTicks >= cpuExitTicks
			exitMem := memOkTicks >= memExitTicks || availMB < 0

			switch {
			case !wasCritical && (enterCPU || enterMem):
				loadIsCritical.Store(true)
				reason := "cpu"
				if enterMem && !enterCPU {
					reason = "mem"
				} else if enterMem && enterCPU {
					reason = "cpu+mem"
				}
				devlog.Logf("load-monitor: ENTER critical  reason=%s  cpu=%d%% avail=%dMB",
					reason, cpuPct, availMB)
				cpuHotTicks = 0
				memLowTicks = 0
			case wasCritical && exitCPU && exitMem:
				loadIsCritical.Store(false)
				devlog.Logf("load-monitor: EXIT critical  cpu=%d%% avail=%dMB",
					cpuPct, availMB)
				cpuCoolTicks = 0
				memOkTicks = 0
			}
		}
	}()
}
