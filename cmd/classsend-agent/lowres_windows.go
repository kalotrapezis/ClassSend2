//go:build windows

package main

import (
	"runtime/debug"
	"syscall"
	"unsafe"

	"classsend/internal/devlog"
)

// Tuning for old / low-resource student PCs (Win7, 2 GB RAM, single-core).
//
//  1. On Win7 BitBlt with CAPTUREBLT forces DWM to re-compose the whole
//     desktop per capture: hundreds of ms on weak GPUs, cursor flicker and
//     sometimes a frozen Aero session. Win7 DWM already includes layered
//     windows in a plain SRCCOPY of the screen DC, so the flag is only
//     needed on Win8+ (where the black-capture bug it fixes exists).
//  2. Keep the Go heap small so the agent is not the first thing paged out
//     and the commit charge stays low on 1–2 GB machines.
//  3. Run slightly above normal priority so lock/mute/shutdown still react
//     when the PC is pegged by a browser.

var (
	ntdll                = syscall.NewLazyDLL("ntdll.dll")
	procRtlGetVersion    = ntdll.NewProc("RtlGetVersion")
	procSetPriorityClass = kernel32.NewProc("SetPriorityClass")

	preWin8 bool
)

type osVersionInfo struct {
	size, major, minor, build, platform uint32
	csd                                 [128]uint16
}

func init() {
	// RtlGetVersion is not affected by manifest-based version lying, unlike GetVersionEx.
	v := osVersionInfo{size: uint32(unsafe.Sizeof(osVersionInfo{}))}
	if r, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&v))); r == 0 {
		preWin8 = v.major < 6 || (v.major == 6 && v.minor < 2)
	}

	debug.SetGCPercent(50)         // collect more often, smaller peak heap
	debug.SetMemoryLimit(48 << 20) // soft cap; GC works harder near it instead of growing

	const aboveNormalPriorityClass = 0x8000
	procSetPriorityClass.Call(^uintptr(0), aboveNormalPriorityClass)
}

// blitFlags returns the raster-op for desktop capture on this OS.
func blitFlags() uintptr {
	if preWin8 {
		return srcCopy
	}
	return srcCopy | captureBlt
}

func logOSTuning() {
	devlog.Logf("os: preWin8=%v blit=%#x gc=50%% memlimit=48MB priority=above-normal", preWin8, blitFlags())
}
