; The Go installer owns the protected files, service, tasks and uninstall entry.
; Inno Setup supplies the wizard and waits for that installer to finish.
#ifndef AppVersion
  #error AppVersion must be supplied with /DAppVersion=X.Y.Z
#endif
#ifndef PayloadDir
  #define PayloadDir "..\dist"
#endif

[Setup]
AppId=HyperHand
AppName=HyperHand
AppVersion={#AppVersion}
AppPublisher=n2ns
AppPublisherURL=https://github.com/n2ns/hyper-hand
VersionInfoVersion={#AppVersion}
ArchitecturesAllowed=x64os
MinVersion=10.0
; Capture the installing user in Go before requesting UAC, including when a
; different administrator's credentials are used. This is still a machine install.
PrivilegesRequired=lowest
CreateAppDir=no
Uninstallable=no
CreateUninstallRegKey=no
DisableWelcomePage=no
DisableProgramGroupPage=yes
CloseApplications=no
RestartApplications=no
SetupIconFile=..\cmd\hyperhand\winres\hyperhand.ico
OutputDir=..\build\installer
OutputBaseFilename=hyperhand-{#AppVersion}-windows-amd64-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern

[Files]
Source: "{#PayloadDir}\hyperhand.exe"; Flags: dontcopy

[Messages]
WelcomeLabel2=This will install or update HyperHand on your computer.%n%nHyperHand requires Windows 10 or 11 Pro or Enterprise with Hyper-V enabled. Administrator approval is required during installation.%n%nFinish active VM requests before continuing. An update restarts HyperHand and disconnects MCP clients.
FinishedLabelNoIcons=HyperHand has been installed. Open its tray settings to copy the MCP address and connect your AI client.%n%nAfter an update, reconnect your MCP client and ask it to run vm_update_agent for each virtual machine.

[Code]
var
  InstallProgress: TOutputMarqueeProgressWizardPage;

procedure InitializeWizard;
begin
  InstallProgress := CreateOutputMarqueeProgressPage('Installing HyperHand',
    'Please wait while HyperHand is installed.');
end;

function UpdateReadyMemo(Space, NewLine, MemoUserInfoInfo, MemoDirInfo,
  MemoTypeInfo, MemoComponentsInfo, MemoGroupInfo, MemoTasksInfo: String): String;
begin
  Result := 'Install location:' + NewLine + Space +
    ExpandConstant('{commonpf64}\HyperHand') + NewLine + NewLine +
    'The background service starts automatically. The tray runs as your ' +
    'normal user and starts at sign-in. An existing startup preference is preserved.';
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ExitCode: Integer;
begin
  Result := '';
  InstallProgress.SetText('Installing the service and tray...',
    'Approve the Windows administrator prompt to continue.');
  InstallProgress.Show;
  try
    InstallProgress.Animate;
    ExtractTemporaryFile('hyperhand.exe');
    if not Exec(ExpandConstant('{tmp}\hyperhand.exe'), 'install --wizard',
      ExpandConstant('{tmp}'), SW_HIDE, ewWaitUntilTerminated, ExitCode) then
      Result := 'Could not start the HyperHand installer: ' + SysErrorMessage(ExitCode)
    else if ExitCode <> 0 then
      Result := Format('HyperHand installation did not complete (exit code %d). ' +
        'If you cancelled administrator approval, try again. Otherwise, check ' +
        'the HyperHand install log in %%LOCALAPPDATA%%\HyperHand\hyperhand.log ' +
        'for the account used to approve installation.', [ExitCode]);
    if Result <> '' then
      Log(Result);
  finally
    InstallProgress.Hide;
  end;
end;
