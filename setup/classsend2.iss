#define MyAppName      "ClassSend 2"
; Default version — overridable via ISCC /DMyAppVersion=... so build-release.bat
; can ship a clean version string without editing this file.
#ifndef MyAppVersion
  #define MyAppVersion "0.0.9"
#endif
#define MyAppPublisher "ClassSend"

; ── Installer metadata ────────────────────────────────────────────────────────
[Setup]
AppId={{B4E2F3A1-7C6D-4F8B-9E2A-3D1C5B7A9F0E}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppContact=kalotrapezis@gmail.com
DefaultDirName={autopf}\ClassSend2
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
OutputDir=..\dist
OutputBaseFilename=ClassSend2-Setup-v{#MyAppVersion}
SetupIconFile=..\assets\ClassSend2.ico
Compression=lzma2/ultra64
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
UninstallDisplayName={#MyAppName}
UninstallDisplayIcon={app}\classsend.exe
AppComments=Ρόλος: {code:GetInstalledRole}

; ── Languages ─────────────────────────────────────────────────────────────────
[Languages]
Name: "en"; MessagesFile: "compiler:Default.isl"

; ── Files ─────────────────────────────────────────────────────────────────────
[Files]
; Teacher
Source: "..\teacher.exe";         DestDir: "{app}"; DestName: "classsend.exe"; \
    Flags: ignoreversion; Check: IsTeacherRole
Source: "..\monitoring.exe";      DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsTeacherRole

; Student — three-tier matrix:
;   Win10+ x64 → 64-bit student.exe + 64-bit agent (Go 1.24)
;   Win10+ x86 → 32-bit student-win10-x86.exe + 32-bit agent-win10-x86.exe
;                (Go 1.24, full chat TUI)
;   Win7/8     → 32-bit Go 1.20 agent only (chat TUI not buildable for Win7)
Source: "..\student.exe";                            DestDir: "{app}"; DestName: "classsend.exe"; \
    Flags: ignoreversion; Check: IsStudentRole and UseModern64
Source: "..\dist\classsend-agent-win10-x64.exe";     DestDir: "{app}"; DestName: "classsend-agent.exe"; \
    Flags: ignoreversion; Check: IsStudentRole and UseModern64
Source: "..\castviewer.exe";                         DestDir: "{app}"; \
    Flags: ignoreversion; Check: IsStudentRole and UseModern64
Source: "..\dist\student-win10-x86.exe";             DestDir: "{app}"; DestName: "classsend.exe"; \
    Flags: ignoreversion; Check: IsStudentRole and UseModern32
Source: "..\dist\classsend-agent-win10-x86.exe";     DestDir: "{app}"; DestName: "classsend-agent.exe"; \
    Flags: ignoreversion; Check: IsStudentRole and UseModern32
Source: "..\dist\castviewer-win10-x86.exe";          DestDir: "{app}"; DestName: "castviewer.exe"; \
    Flags: ignoreversion; Check: IsStudentRole and UseModern32
Source: "..\dist\classsend-agent-win7-x86.exe";      DestDir: "{app}"; DestName: "classsend-agent.exe"; \
    Flags: ignoreversion; Check: IsStudentRole and UseLegacy32
; Win7 has no WebView2 runtime, so no castviewer is shipped — CmdStartCast
; on a Win7 student is a no-op (the agent logs and continues).

; Dev — all four exes, teacher.exe kept as teacher.exe (not renamed). Dev
; install is for testing on the developer's modern PC, so the modern student/
; teacher binaries are unconditional; the legacy agent variant is also
; included so installer-time selection still works under test.
Source: "..\teacher.exe";                        DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole
Source: "..\student.exe";                        DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole
Source: "..\dist\classsend-agent-win10-x64.exe"; DestDir: "{app}"; DestName: "classsend-agent.exe"; \
    Flags: ignoreversion; Check: IsDevRole and UseModern64
Source: "..\dist\classsend-agent-win10-x86.exe"; DestDir: "{app}"; DestName: "classsend-agent.exe"; \
    Flags: ignoreversion; Check: IsDevRole and UseModern32
Source: "..\dist\classsend-agent-win7-x86.exe";  DestDir: "{app}"; DestName: "classsend-agent.exe"; \
    Flags: ignoreversion; Check: IsDevRole and UseLegacy32
Source: "..\monitoring.exe";                     DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole
Source: "..\castviewer.exe";                     DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole

; about.md ships next to every install. The TUI's --about reads this at
; runtime, so its content can be edited post-install without rebuilding.
Source: "..\about.md"; DestDir: "{app}"; Flags: ignoreversion

; ── Teacher Screen Casting (optional, ~200 MB) ────────────────────────────────
; Bundled ffmpeg.exe powers the H.264 cast pipeline (teacher → students). It is
; only meaningful on a teacher install — students decode the stream natively in
; WebView2 with no extra dependency. Gated on the IncludeCasting check, which
; honours the "Teacher Screen Casting" checkbox on the role page (checked by
; default for Teacher / Dev, disabled for Student).
;
; The binary is NOT in git (~200 MB > GitHub's per-file limit). Run
; fetch-ffmpeg.bat at the repo root to download it before building the
; installer. Inno Setup will fail loudly here if the file is missing.
Source: "..\third_party\ffmpeg\ffmpeg.exe";   DestDir: "{app}"; \
    Flags: ignoreversion; Check: IncludeCasting
Source: "..\third_party\ffmpeg\LICENSE.txt";  DestDir: "{app}"; DestName: "ffmpeg-LICENSE.txt"; \
    Flags: ignoreversion; Check: IncludeCasting

; ── Shortcuts ─────────────────────────────────────────────────────────────────
[Icons]
; Teacher
Name: "{autodesktop}\ClassSend 2 (Δάσκαλος)"; Filename: "{app}\classsend.exe"; \
    Comment: "ClassSend 2 — Teacher"; Check: IsTeacherRole

; Student — chat shortcut only on modern systems (where student.exe ships).
Name: "{autodesktop}\ClassSend 2"; Filename: "{app}\classsend.exe"; \
    Comment: "ClassSend 2 — Μαθητής"; Check: IsStudentRole and UseModernAgent

; Dev — separate shortcuts for each exe
Name: "{autodesktop}\ClassSend 2 — Teacher";  Filename: "{app}\teacher.exe"; \
    Parameters: "--dev"; Comment: "ClassSend 2 Teacher (dev mode)"; Check: IsDevRole
Name: "{autodesktop}\ClassSend 2 — Agent";    Filename: "{app}\classsend-agent.exe"; \
    Parameters: "--dev"; Comment: "ClassSend 2 Agent (dev mode)"; Check: IsDevRole
Name: "{autodesktop}\ClassSend 2 — Student";  Filename: "{app}\student.exe"; \
    Parameters: "--dev"; Comment: "ClassSend 2 Student TUI (dev mode)"; Check: IsDevRole

; Start Menu
Name: "{group}\ClassSend 2";                  Filename: "{app}\classsend.exe"; \
    Check: (IsTeacherRole) or (IsStudentRole and UseModernAgent)
Name: "{group}\ClassSend 2 — Teacher";        Filename: "{app}\teacher.exe"; \
    Parameters: "--dev"; Check: IsDevRole
Name: "{group}\ClassSend 2 — Agent (dev)";    Filename: "{app}\classsend-agent.exe"; \
    Parameters: "--dev"; Check: IsDevRole
Name: "{group}\ClassSend 2 — Student (dev)";  Filename: "{app}\student.exe"; \
    Parameters: "--dev"; Check: IsDevRole
Name: "{group}\Uninstall ClassSend 2";        Filename: "{uninstallexe}"

; ── Registry ──────────────────────────────────────────────────────────────────
[Registry]
; Student autostart — agent runs hidden at login
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; \
    ValueType: string; ValueName: "ClassSend"; \
    ValueData: """{app}\classsend-agent.exe"""; \
    Flags: uninsdeletevalue; Check: IsStudentRole

; Dev autostart — also start the agent (with --dev flag)
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; \
    ValueType: string; ValueName: "ClassSend"; \
    ValueData: """{app}\classsend-agent.exe"" --dev"; \
    Flags: uninsdeletevalue; Check: IsDevRole

; Store the installed role for uninstall / repair
Root: HKCU; Subkey: "Software\ClassSend"; ValueType: string; \
    ValueName: "InstalledRole"; ValueData: "{code:GetInstalledRole}"; \
    Flags: uninsdeletekey

; ── Run after install ─────────────────────────────────────────────────────────
[Run]
; Teacher
Filename: "{app}\classsend.exe"; \
    Description: "Launch ClassSend 2 now"; \
    Flags: nowait postinstall skipifsilent; Check: IsTeacherRole

; Student — start agent silently, offer to open TUI (modern systems only)
Filename: "{app}\classsend-agent.exe"; Flags: nowait; Check: IsStudentRole
Filename: "{app}\classsend.exe"; \
    Description: "Open ClassSend 2 chat now"; \
    Flags: nowait postinstall skipifsilent; Check: IsStudentRole and UseModernAgent

; Dev — start agent with --dev, offer to open both sides
Filename: "{app}\classsend-agent.exe"; Parameters: "--dev"; Flags: nowait; Check: IsDevRole
Filename: "{app}\teacher.exe"; \
    Description: "Open Teacher now"; \
    Flags: nowait postinstall skipifsilent; Check: IsDevRole
Filename: "{app}\student.exe"; Parameters: "--dev"; \
    Description: "Open Student TUI now"; \
    Flags: nowait postinstall skipifsilent; Check: IsDevRole

; ── Code — role selection page ────────────────────────────────────────────────
[Code]

function IsWin10OrLater: Boolean;
var
  Ver: TWindowsVersion;
begin
  GetWindowsVersionEx(Ver);
  Result := Ver.Major >= 10;
end;

// Three-tier binary matrix:
//   Win10+ x64 → UseModern64  (64-bit Go 1.24, full TUI)
//   Win10+ x86 → UseModern32  (32-bit Go 1.24, full TUI)
//   Win7/8     → UseLegacy32  (32-bit Go 1.20, agent only)

function UseModern64: Boolean;
begin
  Result := IsWin64 and IsWin10OrLater;
end;

function UseModern32: Boolean;
begin
  Result := (not IsWin64) and IsWin10OrLater;
end;

function UseLegacy32: Boolean;
begin
  Result := not IsWin10OrLater;
end;

// Modern = either bitness on Win10+, where the chat TUI ships.
function UseModernAgent: Boolean;
begin
  Result := IsWin10OrLater;
end;

// Legacy = Win7/8, agent only (no TUI).
function UseLegacyAgent: Boolean;
begin
  Result := not IsWin10OrLater;
end;

var
  RolePage:     TWizardPage;
  TeacherRadio: TNewRadioButton;
  StudentRadio: TNewRadioButton;
  DevRadio:     TNewRadioButton;
  TeacherDesc:  TNewStaticText;
  StudentDesc:  TNewStaticText;
  DevDesc:      TNewStaticText;
  CastCheckbox: TNewCheckBox;
  CastDesc:     TNewStaticText;

function IsTeacherRole: Boolean;
begin
  Result := (RolePage <> nil) and TeacherRadio.Checked;
end;

function IsStudentRole: Boolean;
begin
  Result := (RolePage <> nil) and StudentRadio.Checked;
end;

function IsDevRole: Boolean;
begin
  Result := (RolePage <> nil) and DevRadio.Checked;
end;

function GetInstalledRole(Param: String): String;
begin
  if IsTeacherRole then Result := 'teacher'
  else if IsDevRole then Result := 'dev'
  else                   Result := 'student';
end;

// IncludeCasting gates ffmpeg.exe in [Files]. True when the user has the
// "Teacher Screen Casting" checkbox checked AND their role uses casting
// (Teacher or Dev — Student installs decode the stream natively and don't
// need ffmpeg). The checkbox is auto-disabled when Student is selected so
// this is mostly belt-and-suspenders.
function IncludeCasting: Boolean;
begin
  Result := (CastCheckbox <> nil) and CastCheckbox.Checked
            and (IsTeacherRole or IsDevRole);
end;

// Called when any role radio is clicked. Disables the cast checkbox for
// Student installs (where ffmpeg has no use) and re-enables/restores the
// default for Teacher/Dev.
procedure RoleChanged(Sender: TObject);
begin
  if CastCheckbox = nil then
    Exit;
  if IsStudentRole then begin
    CastCheckbox.Checked := False;
    CastCheckbox.Enabled := False;
    CastDesc.Enabled     := False;
  end else begin
    CastCheckbox.Enabled := True;
    CastCheckbox.Checked := True;
    CastDesc.Enabled     := True;
  end;
end;

procedure CreateRolePage;
begin
  RolePage := CreateCustomPage(
    wpWelcome,
    'Select Role',
    'Choose the role for this computer.');

  { ── Teacher ── }
  TeacherRadio := TNewRadioButton.Create(RolePage);
  with TeacherRadio do begin
    Caption    := 'Teacher';
    Top        := 8;
    Left       := 16;
    Width      := RolePage.SurfaceWidth - 16;
    Parent     := RolePage.Surface;
    Font.Style := [fsBold];
  end;

  TeacherDesc := TNewStaticText.Create(RolePage);
  with TeacherDesc do begin
    Caption := 'Installs teacher.exe + monitoring.exe. Creates a desktop shortcut.';
    Top     := 28;
    Left    := 32;
    Width   := RolePage.SurfaceWidth - 32;
    Parent  := RolePage.Surface;
  end;

  { ── Student ── }
  StudentRadio := TNewRadioButton.Create(RolePage);
  with StudentRadio do begin
    Caption    := 'Student';
    Top        := 58;
    Left       := 16;
    Width      := RolePage.SurfaceWidth - 16;
    Parent     := RolePage.Surface;
    Font.Style := [fsBold];
    Checked    := True;
  end;

  StudentDesc := TNewStaticText.Create(RolePage);
  with StudentDesc do begin
    Caption := 'Installs student.exe + classsend-agent.exe. Agent starts at login (autostart).';
    Top     := 78;
    Left    := 32;
    Width   := RolePage.SurfaceWidth - 32;
    Parent  := RolePage.Surface;
  end;

  { ── Dev ── }
  DevRadio := TNewRadioButton.Create(RolePage);
  with DevRadio do begin
    Caption    := 'Developer / Testing';
    Top        := 108;
    Left       := 16;
    Width      := RolePage.SurfaceWidth - 16;
    Parent     := RolePage.Surface;
    Font.Style := [fsBold];
  end;

  DevDesc := TNewStaticText.Create(RolePage);
  with DevDesc do begin
    Caption := 'Installs all four exes on one machine. Shortcuts launch in --dev mode so ' +
               'teacher and student can run on the same PC. Agent also starts at login.';
    Top     := 128;
    Left    := 32;
    Width   := RolePage.SurfaceWidth - 32;
    Parent  := RolePage.Surface;
  end;

  { ── Teacher Screen Casting (optional dependency) ── }
  CastCheckbox := TNewCheckBox.Create(RolePage);
  with CastCheckbox do begin
    Caption := 'Teacher Screen Casting';
    Top     := 168;
    Left    := 16;
    Width   := RolePage.SurfaceWidth - 16;
    Parent  := RolePage.Surface;
    Font.Style := [fsBold];
    Checked := True;
  end;

  CastDesc := TNewStaticText.Create(RolePage);
  with CastDesc do begin
    Caption := 'Adds ~200 MB. Bundles ffmpeg.exe so the teacher can broadcast their ' +
               'screen as live video to students. Only useful on a teacher install — ' +
               'student installs decode the stream natively without it.';
    Top     := 188;
    Left    := 32;
    Width   := RolePage.SurfaceWidth - 32;
    Height  := 36;
    AutoSize := False;
    WordWrap := True;
    Parent  := RolePage.Surface;
  end;

  { Wire radio buttons so toggling role updates the checkbox state. }
  TeacherRadio.OnClick := @RoleChanged;
  StudentRadio.OnClick := @RoleChanged;
  DevRadio.OnClick     := @RoleChanged;

  { Apply initial state — Student is the default radio so cast box should
    start disabled, but RoleChanged will set it correctly. }
  RoleChanged(nil);
end;

procedure InitializeWizard;
begin
  CreateRolePage;
end;
