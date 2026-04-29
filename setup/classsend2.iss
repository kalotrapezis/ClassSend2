#define MyAppName      "ClassSend 2"
#define MyAppVersion   "0.0.1"
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

; Student
Source: "..\student.exe";         DestDir: "{app}"; DestName: "classsend.exe"; \
    Flags: ignoreversion; Check: IsStudentRole
Source: "..\classsend-agent.exe"; DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsStudentRole

; Dev — all four exes, teacher.exe kept as teacher.exe (not renamed)
Source: "..\teacher.exe";         DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole
Source: "..\student.exe";         DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole
Source: "..\classsend-agent.exe"; DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole
Source: "..\monitoring.exe";      DestDir: "{app}"; Flags: ignoreversion; \
    Check: IsDevRole

; ── Shortcuts ─────────────────────────────────────────────────────────────────
[Icons]
; Teacher
Name: "{autodesktop}\ClassSend 2 (Δάσκαλος)"; Filename: "{app}\classsend.exe"; \
    Comment: "ClassSend 2 — Teacher"; Check: IsTeacherRole

; Student
Name: "{autodesktop}\ClassSend 2"; Filename: "{app}\classsend.exe"; \
    Comment: "ClassSend 2 — Μαθητής"; Check: IsStudentRole

; Dev — separate shortcuts for each exe
Name: "{autodesktop}\ClassSend 2 — Teacher";  Filename: "{app}\teacher.exe"; \
    Parameters: "--dev"; Comment: "ClassSend 2 Teacher (dev mode)"; Check: IsDevRole
Name: "{autodesktop}\ClassSend 2 — Agent";    Filename: "{app}\classsend-agent.exe"; \
    Parameters: "--dev"; Comment: "ClassSend 2 Agent (dev mode)"; Check: IsDevRole
Name: "{autodesktop}\ClassSend 2 — Student";  Filename: "{app}\student.exe"; \
    Parameters: "--dev"; Comment: "ClassSend 2 Student TUI (dev mode)"; Check: IsDevRole

; Start Menu
Name: "{group}\ClassSend 2";                  Filename: "{app}\classsend.exe"; \
    Check: not IsDevRole
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

; Student — start agent silently, offer to open TUI
Filename: "{app}\classsend-agent.exe"; Flags: nowait; Check: IsStudentRole
Filename: "{app}\classsend.exe"; \
    Description: "Open ClassSend 2 chat now"; \
    Flags: nowait postinstall skipifsilent; Check: IsStudentRole

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

var
  RolePage:    TWizardPage;
  TeacherRadio: TNewRadioButton;
  StudentRadio: TNewRadioButton;
  DevRadio:     TNewRadioButton;
  TeacherDesc:  TNewStaticText;
  StudentDesc:  TNewStaticText;
  DevDesc:      TNewStaticText;

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
end;

procedure InitializeWizard;
begin
  CreateRolePage;
end;
