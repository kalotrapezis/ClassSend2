# Windows build — v0.2.3 (TODO: build + installer on the VM)

The code for **0.2.3 is ready**. The only thing left for the Windows release is to
run the build on the Windows VM — no further code changes needed.

## What changed since 0.2.2

- **Version bumped to `0.2.3`** — already set in `build.bat` (`set VERSION=0.2.3`),
  so every `.exe` and the installer report 0.2.3.
- **TUI ghost-frame fix** (`internal/tui/model.go`). This is the fix for the bug
  in the classroom photo: the teacher sidebar showing up **twice / "two
  groupings"** and the monitoring + user list **"pushing everything up"**. On a
  narrow terminal the 11-entry shortcut bar wrapped to a 2nd line, making the
  frame one row too tall; in alt-screen mode that scrolls and leaves a ghost of
  the previous render. The bar is now capped at one line, the view is clamped to
  the terminal height, and the staged-file row is budgeted in. **This affects the
  Windows teacher** (that's where the bug was seen), so it's worth shipping.
- Push-open / file auto-open refactored to a cross-platform helper
  (`internal/core/open.go`); on Windows it still calls `cmd /c start`, i.e. no
  behaviour change — just no longer hard-coded.

Full notes: see the `## [0.2.3-Linux]` section in `CHANGELOG.md`.

## Where the code is

All of the above is on the **`linux`** branch (PR #8, already merged). The
Linux-only files are build-tagged (`//go:build linux`) so they are **invisible to
the Windows build** — building this branch on Windows produces the normal Windows
artifacts. Build from this branch directly, or merge/cherry-pick it into `master`
first if you'd rather release Windows from `master`.

## Steps on the Windows VM

1. Get this branch: `git fetch && git switch linux` (or merge it into `master`).
2. Prerequisites (one-time):
   - **Inno Setup 6** installed (for the installer step).
   - **ffmpeg.exe** present — run `fetch-ffmpeg.bat` if it's missing. Only the
     optional "Teacher Screen Casting" installer component needs it.
   - **Win7 agent** `dist\classsend-agent-win7-x86.exe` present if you ship the
     Win7 build — produced by `build-win7.bat` (separate, older toolchain).
3. Run **`build.bat`**. It builds `teacher.exe`, `student.exe`,
   `classsend-agent.exe`, `monitoring.exe`, `castviewer.exe`, the Win10 x86 pair,
   and then the Inno Setup installer.
4. Output: **`dist\ClassSend2-Setup-v0.2.3.exe`**.

## Already done (for reference)

The Linux side is fully released: **v0.2.3-Linux** on GitHub with `.rpm` + `.deb`
assets. Nothing to do there.
