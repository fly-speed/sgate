#if Architecture == "amd64"
  #define NativeArchitecture "x64os"
#elif Architecture == "arm64"
  #define NativeArchitecture "arm64"
#else
  #error Unsupported Windows client architecture
#endif

[Setup]
AppId=sgate-client
AppName=sgate Client
AppVersion={#Version}
DefaultDirName={autopf}\sgate
DisableDirPage=yes
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed={#NativeArchitecture}
ArchitecturesInstallIn64BitMode={#NativeArchitecture}
UninstallDisplayIcon={app}\bin\sgate-control.exe
WizardStyle=modern
RestartApplications=no

[Tasks]
Name: "desktopicon"; Description: "Create a desktop shortcut"; Flags: unchecked

[Files]
Source: "{#Bundle}\bin\*"; DestDir: "{app}\bin"; Flags: ignoreversion
Source: "{#Bundle}\web\admin\*"; DestDir: "{app}\web\admin"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "{#Bundle}\docs\*"; DestDir: "{app}\docs"; Flags: ignoreversion
Source: "{#Bundle}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion
Source: "..\LICENSE"; DestDir: "{app}\licenses"; DestName: "sgate-LICENSE.txt"; Flags: ignoreversion
Source: "{#Bundle}\conf\sgate-client.cf"; DestDir: "{app}\conf"; Flags: onlyifdoesntexist uninsneveruninstall
Source: "{#Bundle}\conf\tls-cert.pem"; DestDir: "{app}\conf"; Flags: onlyifdoesntexist uninsneveruninstall

[Icons]
Name: "{commonprograms}\sgate Client"; Filename: "{app}\bin\sgate-control.exe"; WorkingDir: "{app}"
Name: "{commondesktop}\sgate Client"; Filename: "{app}\bin\sgate-control.exe"; WorkingDir: "{app}"; Tasks: desktopicon

[Run]
Filename: "{app}\bin\sgate-control.exe"; Description: "Open sgate Client"; Flags: postinstall skipifsilent nowait unchecked

[Code]
const
  HexDigits = '0123456789abcdef';

type
  TKeyBytes = array[0..31] of Byte;

function BCryptGenRandom(Algorithm: Integer; var Buffer: TKeyBytes;
  BufferSize, Flags: Cardinal): Integer;
  external 'BCryptGenRandom@bcrypt.dll stdcall setuponly';

function ControllerIsRunning: Boolean;
var
  RootHash: String;
begin
  { Match the per-installation mutex in cmd/sgate-control/main_windows.go. }
  RootHash := GetSHA256OfString(UTF8Encode(Lowercase(ExpandConstant('{app}'))));
  Result := CheckForMutexes('Local\sgate-control-' + Copy(Lowercase(RootHash), 1, 24));
end;

function ProtectDirectory(Path: String): Boolean;
var
  ExitCode: Integer;
begin
  Result := ForceDirectories(Path) and
    Exec(ExpandConstant('{sys}\icacls.exe'),
      '"' + Path + '" /grant:r *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F /inheritance:r',
      '', SW_HIDE, ewWaitUntilTerminated, ExitCode) and (ExitCode = 0);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  KeyPath, Key: String;
  Bytes: TKeyBytes;
  I: Integer;
begin
  Result := '';
  if CompareText(ExpandConstant('{app}'), ExpandConstant('{autopf}\sgate')) <> 0 then
    Result := 'Install sgate under Program Files using the default destination.'
  else if ControllerIsRunning then
    Result := 'Close the installed sgate Client window before continuing.'
  else if not ProtectDirectory(ExpandConstant('{app}\conf')) or
          not ProtectDirectory(ExpandConstant('{app}\logs')) then
    Result := 'Could not protect the sgate configuration and log directories.'
  else begin
    KeyPath := ExpandConstant('{app}\conf\sgate.key');
    if not FileExists(KeyPath) then begin
      if BCryptGenRandom(0, Bytes, 32, 2) <> 0 then begin
        Result := 'Could not generate a local placeholder key.';
        Exit;
      end;
      Key := '';
      for I := 0 to 31 do
        Key := Key + Copy(HexDigits, Bytes[I] div 16 + 1, 1) +
          Copy(HexDigits, Bytes[I] mod 16 + 1, 1);
      if not SaveStringToFile(KeyPath, Key, False) then
        Result := 'Could not write the local placeholder key.';
    end;
  end;
end;

function InitializeUninstall: Boolean;
begin
  Result := not ControllerIsRunning;
  if not Result then
    SuppressibleMsgBox('Close the installed sgate Client window, then run uninstall again.',
      mbError, MB_OK, IDOK);
end;
