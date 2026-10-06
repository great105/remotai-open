; Remotai — классический per-user инсталлятор (Inno Setup 6).
;
; Сборка:  ISCC.exe /DAppVersion=2.6.0 installer\remotai.iss
;          (вызывается из scripts/publish-release.ps1; вход — build\remotai.exe)
; Выход:   build\remotai-setup.exe
;
; Решения:
;  - PrivilegesRequired=lowest + {localappdata}\Programs\Remotai: без UAC,
;    папка user-writable — автообновление exe (internal/update) продолжает
;    работать как в portable-режиме. Папку из страницы выбора проверяем на
;    запись (DirWritable): Program Files тихо убил бы автообновление навсегда.
;  - Автозапуск — через сам exe (--enable-autostart): создаётся та же
;    scheduled task (TGControlUser), которую видит и переключает UI настроек.
;  - Перед апгрейдом/удалением убиваем ТОЛЬКО процессы из {app} — чтобы не
;    задеть другие копии (dev runtime, portable). Фильтр по НАЧАЛУ пути exe,
;    поэтому под него попадают и пережившие автообновление pty-host'ы,
;    работающие из переименованной копии remotai.exe.old-<ms>.
;  - Апгрейд поверх живых персистентных терминалов: их процесс держит образ
;    exe открытым, поэтому перед [Files] переименовываем работающий файл
;    (тот же приём, что в internal/update) — иначе установка падает системной
;    ошибкой «файл используется другой программой».

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif

#ifndef InstallerAppId
  #define InstallerAppId "{{AB80AC27-27C1-4830-919E-0D83D1980C72}"
#endif
#ifndef InstallerAppName
  #define InstallerAppName "Remotai"
#endif

[Setup]
AppId={#InstallerAppId}
AppName={#InstallerAppName}
AppVersion={#AppVersion}
AppVerName=Remotai {#AppVersion}
AppPublisher=Remotai
AppPublisherURL=https://remotai.ru
AppSupportURL=https://remotai.ru
DefaultDirName={localappdata}\Programs\{#InstallerAppName}
DisableDirPage=no
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
OutputDir=..\build
OutputBaseFilename=remotai-setup
SetupIconFile=remotai.ico
; Свои картинки вместо стоковых Inno (коробка с компакт-диском): первое, что
; человек видит после скачивания, — это окно установщика, и оно должно
; выглядеть тем же продуктом, что и сайт (аудит онбординга 30.08.2026).
WizardImageFile=wizard-large.bmp
WizardSmallImageFile=wizard-small.bmp
WizardImageStretch=no
UninstallDisplayIcon={app}\remotai.ico
UninstallDisplayName={#InstallerAppName}
VersionInfoVersion={#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
CloseApplications=no
; Язык берём у системы и НЕ спрашиваем: диалог выбора языка — лишний экран
; перед установкой, а про свой язык система знает точно (аудит онбординга
; 30.08.2026). Выбрать другой по-прежнему можно ключом /LANG=english.
ShowLanguageDialog=no
LanguageDetectionMethod=uilanguage

[Languages]
Name: "russian"; MessagesFile: "compiler:Languages\Russian.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
russian.AutostartTask=Запускать Remotai при входе в Windows — иначе после перезагрузки компьютер пропадёт из приложения
english.AutostartTask=Start Remotai when you sign in to Windows — otherwise the computer disappears from the app after a reboot
russian.StartupGroup=Работа после перезагрузки:
english.StartupGroup=After a reboot:
russian.TerminalShortcutComment=Открыть терминал этого компьютера — он будет доступен с других устройств
english.TerminalShortcutComment=Open a terminal on this computer — also accessible from other devices
russian.TerminalShortcutName=Терминал
english.TerminalShortcutName=Terminal
russian.TerminalShortcutTask=Добавить отдельный ярлык «Remotai Терминал» в меню «Пуск» (для работы с командами)
english.TerminalShortcutTask=Add a separate Remotai Terminal shortcut to Start (for command-line work)
russian.AutostartFailed=Remotai установлен, но включить автозапуск не удалось.%n%nОткройте Remotai → «Панель ПК» → «Автозапуск» и попробуйте ещё раз. До этого запускайте приложение вручную после входа в Windows.
english.AutostartFailed=Remotai is installed, but automatic startup could not be enabled.%n%nOpen Remotai → PC panel → Autostart and try again. Until then, open the app manually after signing in to Windows.
russian.DirNotWritable=Remotai не сможет писать в папку «%1».%n%nВ системные папки (Program Files и подобные) программа без прав администратора писать не может: установка либо прервётся, либо Remotai навсегда потеряет способность обновляться сам — он заменяет свой файл рядом с собой.%n%nВыберите папку внутри своего профиля — например, предложенную по умолчанию.
english.DirNotWritable=Remotai cannot write to "%1".%n%nWithout administrator rights the program cannot write to system folders (Program Files and the like): the installation will either fail or Remotai will permanently lose the ability to update itself, because it replaces its own file in place.%n%nPick a folder inside your user profile — for example the suggested default.
russian.ClosePtyAsk=Файл программы занят работающими терминалами Remotai и не поддался замене.%n%nЗакрыть эти терминалы и продолжить установку? Всё несохранённое в них будет потеряно.
english.ClosePtyAsk=The program file is held by running Remotai terminals and could not be replaced.%n%nClose those terminals and continue the installation? Anything unsaved in them will be lost.
russian.ExeLocked=Не удалось заменить файл программы: он занят другой копией Remotai.%n%nЗакройте терминалы Remotai (или перезагрузите компьютер) и запустите установку снова.
english.ExeLocked=Could not replace the program file: it is in use by another copy of Remotai.%n%nClose the Remotai terminals (or restart the computer) and run the installation again.
russian.UnpairAsk=Отвязать этот компьютер от аккаунта Remotai?%n%n«Да» — компьютер сразу исчезнет из приложения на телефонах, его настройки и журналы будут удалены.%n%n«Нет» — привязка, настройки и журналы останутся на диске: после повторной установки компьютер сам вернётся в приложение, новый QR-код не понадобится.
english.UnpairAsk=Unlink this computer from your Remotai account?%n%n"Yes" — the computer disappears from the phone app right away, and its settings and logs are deleted.%n%n"No" — the pairing, settings and logs stay on disk: after reinstalling, the computer comes back to the app on its own, no new QR code needed.

[Tasks]
; ⚠ У автозапуска СВОЯ группа. Он стоял под заголовком «Дополнительные значки»,
; и человек снимал галочку, читая её как «лишний ярлык», — а вместе с ней
; компьютер пропадал из приложения после каждой перезагрузки (аудит онбординга
; 30.08.2026). Последствие названо прямо в подписи.
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"
Name: "terminalicon"; Description: "{cm:TerminalShortcutTask}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked
Name: "autostart"; Description: "{cm:AutostartTask}"; GroupDescription: "{cm:StartupGroup}"

[Files]
Source: "..\build\remotai.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\build\remotai.com"; DestDir: "{app}"; Flags: ignoreversion
Source: "remotai.ico"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#InstallerAppName}"; Filename: "{app}\remotai.exe"; IconFilename: "{app}\remotai.ico"
; Терминал, сразу подключённый к Remotai-сессии (виден с телефона).
; Подпись обязательна: в «Пуске» это вторая строка с тем же именем, и что она
; делает, человеку не объяснял никто (аудит онбординга 30.08.2026).
Name: "{autoprograms}\{#InstallerAppName} {cm:TerminalShortcutName}"; Filename: "{app}\remotai.exe"; Parameters: "attach --new"; WorkingDir: "{%USERPROFILE}"; IconFilename: "{app}\remotai.ico"; Comment: "{cm:TerminalShortcutComment}"; Tasks: terminalicon
Name: "{autodesktop}\{#InstallerAppName}"; Filename: "{app}\remotai.exe"; IconFilename: "{app}\remotai.ico"; Tasks: desktopicon

[Run]
Filename: "{app}\remotai.exe"; Description: "{cm:LaunchProgram,Remotai}"; Flags: nowait postinstall skipifsilent

[UninstallDelete]
; Хвосты автообновления (internal/update): .old/.new/.update.bat рядом с exe.
; Имя бэкапа с 2.26 уникально (`.old-<ms>`, см. update.Apply) — маска обязательна,
; иначе после удаления в папке остаётся копия exe на 25+ МБ.
Type: files; Name: "{app}\remotai.exe.old"
Type: files; Name: "{app}\remotai.exe.old-*"
Type: files; Name: "{app}\remotai.exe.prev*"
Type: files; Name: "{app}\remotai.exe.new"
Type: files; Name: "{app}\remotai.exe.update.bat"
Type: dirifempty; Name: "{app}"

[Code]
var
  { Ответ на вопрос деинсталлятора: отзывать ли устройство из аккаунта и
    удалять ли его данные. Тихое удаление ведёт себя как раньше: отзыв есть,
    данные не трогаем. }
  UnlinkAccount: Boolean;
  RemoveUserData: Boolean;

// [Run] ignores the child's failure status. Show a useful recovery path when
// Windows rejects task registration instead of claiming successful autostart.
procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if (CurStep = ssPostInstall) and WizardIsTaskSelected('autostart') then begin
    if not Exec(ExpandConstant('{app}\remotai.exe'), '--enable-autostart', '',
                SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then begin
      Log('Remotai: autostart registration failed');
      SuppressibleMsgBox(CustomMessage('AutostartFailed'), mbError, MB_OK, IDOK);
    end;
  end;
end;

// AppProcessFilter — PS-конвейер, отбирающий процессы, запущенные из {app}.
// Сравниваем по НАЧАЛУ пути exe, поэтому под фильтр попадают и переименованные
// автообновлением копии (remotai.exe.old-<ms>) — из них живут pty-host'ы,
// пережившие обновление; фильтр по точному пути их не видел, и после удаления
// программы такой процесс продолжал работать с пользовательскими шеллами.
// Другие копии Remotai (dev runtime, portable в Downloads) не трогаем.
function AppProcessFilter(): String;
var
  Prefix: String;
begin
  Prefix := ExpandConstant('{app}') + '\remotai.exe';
  StringChangeEx(Prefix, '''', '''''', True);
  Result := 'Get-CimInstance Win32_Process | '
    + 'Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith(''' + Prefix + ''', '
    + '[System.StringComparison]::OrdinalIgnoreCase) }';
end;

// Убивает только процессы remotai, запущенные из {app}. При ОБНОВЛЕНИИ сохраняем
// --pty-host: персистентные терминалы переживают замену exe так же, как при
// встроенном автообновлении. При полном удалении их закрываем.
procedure KillInstalledApp(PreservePtyHosts: Boolean);
var
  ResultCode: Integer;
  Cmd: String;
begin
  Cmd := '-NoProfile -Command "' + AppProcessFilter();
  if PreservePtyHosts then
    Cmd := Cmd + ' | Where-Object { $_.CommandLine -notmatch ''--pty-host'' }';
  Cmd := Cmd + ' | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }"';
  Exec('powershell.exe', Cmd, '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

// CountAppProcesses — сколько процессов сейчас работает из {app} (см.
// AppProcessFilter). Возвращает -1, если запустить проверку не удалось.
function CountAppProcesses(): Integer;
var
  ResultCode: Integer;
  Cmd: String;
begin
  { Код возврата = число процессов: @(...) даёт массив даже из одного объекта. }
  Cmd := '-NoProfile -Command "exit @(' + AppProcessFilter() + ').Count"';
  if Exec('powershell.exe', Cmd, '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    Result := ResultCode
  else
    Result := -1;
end;

// UnderDir — Path лежит внутри Parent (без учёта регистра).
function UnderDir(Path, Parent: String): Boolean;
begin
  Result := False;
  if Parent = '' then
    exit;
  Parent := AddBackslash(Parent);
  Result := CompareText(Copy(AddBackslash(Path), 1, Length(Parent)), Parent) = 0;
end;

// InSystemDir — папка внутри системной (Program Files, Windows). Пробной записи
// тут недостаточно: установщик, запущенный «от имени администратора», в Program
// Files запишет — а вот агент, работающий потом под обычным пользователем, свой
// exe при автообновлении заменить уже не сможет и молча застрянет на этой версии.
function InSystemDir(Dir: String): Boolean;
begin
  Result := UnderDir(Dir, ExpandConstant('{commonpf32}'))
         or UnderDir(Dir, ExpandConstant('{commonpf64}'))
         or UnderDir(Dir, ExpandConstant('{win}'));
end;

// DirUsable — сможет ли Remotai писать в каталог, работая под обычным
// пользователем. Идём вверх до первого существующего родителя и пробуем создать
// там файл (папку самого приложения установщик ещё не создал). Так отсекается
// Program Files, куда установка либо не пройдёт, либо пройдёт и НАВСЕГДА сломает
// автообновление — оно заменяет exe рядом с собой (internal/update).
function DirUsable(Dir: String): Boolean;
var
  Existing, Parent, Probe: String;
begin
  if InSystemDir(Dir) then begin
    Result := False;
    exit;
  end;
  Existing := Dir;
  while not DirExists(Existing) do begin
    Parent := ExtractFileDir(Existing);
    if (Parent = '') or (CompareText(Parent, Existing) = 0) then begin
      Result := False;
      exit;
    end;
    Existing := Parent;
  end;
  Probe := AddBackslash(Existing) + 'remotai-write-test.tmp';
  Result := SaveStringToFile(Probe, 'ok', False);
  if Result then
    DeleteFile(Probe);
end;

// Never silently replace the selected directory. CurPageChanged also runs
// during /VERYSILENT: replacing a rejected /DIR with the default could update
// an entirely different live installation. Explain the error and stop instead.

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  if (CurPageID = wpSelectDir) and not WizardSilent() then begin
    if not DirUsable(WizardDirValue) then begin
      MsgBox(FmtMessage(CustomMessage('DirNotWritable'), [WizardDirValue]), mbError, MB_OK);
      Result := False;
    end;
  end;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  AppExe: String;
begin
  Result := '';

  { Тихая установка и /DIR= страницу выбора не проходят — проверяем ещё раз
    здесь, чтобы «успешная» установка не оказалась установкой без обновлений. }
  if not DirUsable(ExpandConstant('{app}')) then begin
    Result := FmtMessage(CustomMessage('DirNotWritable'), [ExpandConstant('{app}')]);
    exit;
  end;

  AppExe := ExpandConstant('{app}\remotai.exe');
  if not FileExists(AppExe) then
    exit;

  KillInstalledApp(True);
  Sleep(800);

  // Остался живой процесс из папки установки — это персистентный pty-host (его
  // мы щадим осознанно: терминалы должны пережить обновление). Он держит образ
  // exe открытым, и [Files] упёрся бы в системную ошибку «файл используется».
  // Работающий образ Windows разрешает ПЕРЕИМЕНОВАТЬ — тот же приём, что в
  // internal/update: хост доживает из .old-файла, на место exe ложится новая
  // версия, а бэкап подметает update.CleanupOldBinary при следующем старте.
  if CountAppProcesses() <> 0 then begin
    if not RenameFile(AppExe, AppExe + '.old-' + GetDateTimeString('yyyymmddhhnnss', #0, #0)) then begin
      if not WizardSilent() then
        if MsgBox(CustomMessage('ClosePtyAsk'), mbConfirmation, MB_YESNO) = IDYES then begin
          KillInstalledApp(False);
          Sleep(1200);
        end;
      if CountAppProcesses() <> 0 then begin
        Result := CustomMessage('ExeLocked');
        exit;
      end;
    end;
  end;
end;

{ Вопрос про отвязку задаём ДО каких-либо действий: раньше устройство молча
  отзывалось на шаге удаления, а уже после этого предлагалось «сохранить
  привязку компьютера к аккаунту» — сохранять было нечего. }
function InitializeUninstall(): Boolean;
var
  Answer: Integer;
begin
  Result := True;
  UnlinkAccount := True;
  RemoveUserData := False;
  if UninstallSilent() then
    exit;
  Answer := MsgBox(CustomMessage('UnpairAsk'), mbConfirmation, MB_YESNOCANCEL or MB_DEFBUTTON1);
  if Answer = IDCANCEL then begin
    Result := False;
    exit;
  end;
  UnlinkAccount := (Answer = IDYES);
  RemoveUserData := UnlinkAccount;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
  CleanupArgs: String;
begin
  if CurUninstallStep = usUninstall then begin
    { Чистим self-copy/PATH и (если пользователь согласился) отзываем
      устройство: телефоны сразу теряют к нему доступ. С --keep-pairing
      привязка остаётся — после повторной установки компьютер вернётся сам. }
    CleanupArgs := '--uninstall-cleanup';
    if not UnlinkAccount then
      CleanupArgs := CleanupArgs + ' --keep-pairing';
    Exec(ExpandConstant('{app}\remotai.exe'), CleanupArgs, '', SW_HIDE,
         ewWaitUntilTerminated, ResultCode);
    Exec(ExpandConstant('{app}\remotai.exe'), '--disable-autostart', '', SW_HIDE,
         ewWaitUntilTerminated, ResultCode);
    KillInstalledApp(False);
    Sleep(500);
  end;
  if CurUninstallStep = usPostUninstall then begin
    if RemoveUserData then
      DelTree(ExpandConstant('{localappdata}\Remotai'), True, True, True);
  end;
end;
