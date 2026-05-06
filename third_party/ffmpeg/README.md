# Bundled ffmpeg.exe

`ffmpeg.exe` in this directory is shipped with the teacher install to power
the H.264 cast pipeline (see [internal/network/cast.go](../../internal/network/cast.go)).

## Source

Downloaded from BtbN's pre-built FFmpeg releases:

- Release: <https://github.com/BtbN/FFmpeg-Builds/releases/latest>
- Asset:   `ffmpeg-master-latest-win64-gpl.zip` → `bin/ffmpeg.exe`
- License: GPL 3 (libx264 is GPL — see `LICENSE.txt` in this folder)

The zip archive itself is **not** committed (the binary is ~200 MB, well above
GitHub's 100 MB per-file limit). Run [`fetch-ffmpeg.bat`](../../fetch-ffmpeg.bat)
at the repo root to download and extract it, or grab the asset manually and
copy `ffmpeg.exe` + `LICENSE.txt` into this directory.

## Why bundle?

Without ffmpeg, the teacher's `--cast` / "tvon" feature is disabled and the
log says "ffmpeg.exe not found". Bundling makes a fresh ClassSend2 install
work for casting out of the box. The cost is installer size: the GPL build
is ~200 MB on disk and adds ~100 MB to the compressed installer.

The installer exposes this as the **"Teacher Screen Casting"** component —
checked by default but easy to opt out of for installs that don't need it
(e.g. student-only PCs, teacher PCs that won't broadcast). Toggle it during
Inno Setup's "Select Components" step.

## Why this specific build?

- **GPL static** (not LGPL): we need libx264, which is GPL-licensed.
- **Static** (not shared): single self-contained .exe — no extra DLLs to
  bundle, no shared-runtime conflicts on student PCs.
- **BtbN** rather than Gyan: BtbN ships nightly static builds; Gyan's
  "essentials" was the smaller comparable but its size and provenance were
  similar. Either would work here.

## Updating

When a future ffmpeg fixes a bug we care about, re-run `fetch-ffmpeg.bat`.
It always pulls `latest` from BtbN. Bump the ClassSend2 version and rebuild
the installer. The wire format between teacher and student doesn't depend on
the exact ffmpeg version — anything libx264 + mp4 muxer + rawvideo demuxer
will work.
