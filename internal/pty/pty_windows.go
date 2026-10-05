//go:build windows

package pty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procUpdateProcThreadAttribute = kernel32.NewProc("UpdateProcThreadAttribute")
	procPeekNamedPipe             = kernel32.NewProc("PeekNamedPipe")
)

// conPTY wraps a Windows pseudo console (ConPTY).
type conPTY struct {
	hPC        syscall.Handle
	inPipe     *os.File // write here → PTY stdin
	outPipe    *os.File // read here ← PTY stdout
	procHandle syscall.Handle
	procPID    uint32 // PID of the shell process — used for foreground enumeration
	// Job Object с KILL_ON_JOB_CLOSE, куда посажен шелл до первой инструкции
	// (CREATE_SUSPENDED → Assign → Resume). Держит ВСЁ дерево потомков: закрытие
	// терминала обязано убивать и detached-процессы (codex спавнит node_repl/
	// MCP-серверы вне консоли — ClosePseudoConsole и TerminateProcess шелла их
	// не достают, сироты копились сотнями). Смерть pty-host тоже закрывает
	// хэндл → ОС сама добивает дерево. 0 = джоб создать не удалось (living
	// degraded: старое поведение).
	job windows.Handle
}

// newPlatformPTY creates a ConPTY and spawns a shell process inside it.
// argv — готовая командная строка (argv[0] — путь к шеллу): при выключенной
// разметке команд это ровно [shell], то есть прежний запуск; extraEnv ложится
// поверх окружения (shellLaunch, ST-10).
func newPlatformPTY(cols, rows int, cwd string, argv, extraEnv []string) (*conPTY, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("не задан шелл")
	}
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}

	// Create two pipe pairs.
	// Input:  we write to inW  → PTY reads from inR
	// Output: PTY writes to outW → we read from outR
	var inR, inW, outR, outW syscall.Handle
	if err := syscall.CreatePipe(&inR, &inW, nil, 65536); err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	if err := syscall.CreatePipe(&outR, &outW, nil, 65536); err != nil {
		syscall.CloseHandle(inR)
		syscall.CloseHandle(inW)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}

	var hPC windows.Handle
	if err := windows.CreatePseudoConsole(
		windows.Coord{X: int16(cols), Y: int16(rows)},
		windows.Handle(inR),
		windows.Handle(outW),
		0,
		&hPC,
	); err != nil {
		syscall.CloseHandle(inR)
		syscall.CloseHandle(inW)
		syscall.CloseHandle(outR)
		syscall.CloseHandle(outW)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}

	procHandle, procPID, job, err := spawnInPseudoConsole(hPC, cwd, argv, extraEnv)
	// Keep these handles alive until after CreateProcess attaches the child to
	// the pseudoconsole. Closing them earlier leaves ConPTY with broken pipes.
	syscall.CloseHandle(inR)
	syscall.CloseHandle(outW)
	if err != nil {
		windows.ClosePseudoConsole(hPC)
		syscall.CloseHandle(inW)
		syscall.CloseHandle(outR)
		return nil, err
	}

	return &conPTY{
		hPC:        syscall.Handle(hPC),
		inPipe:     os.NewFile(uintptr(inW), "|pty-in"),
		outPipe:    os.NewFile(uintptr(outR), "|pty-out"),
		procHandle: procHandle,
		procPID:    procPID,
		job:        job,
	}, nil
}

// makeKillOnCloseJob создаёт Job Object, при закрытии последнего хэндла
// которого ОС убивает все процессы джоба (и их потомков — они наследуют джоб,
// вырваться можно только с CREATE_BREAKAWAY, который мы не разрешаем).
// Возвращает 0 при ошибке — вызывающий живёт без джоба.
func makeKillOnCloseJob() windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return 0
	}
	return job
}

func spawnInPseudoConsole(hPC windows.Handle, cwd string, argv, extraEnv []string) (syscall.Handle, uint32, windows.Handle, error) {
	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	defer attrList.Delete()

	if err := updateProcThreadAttributeValue(
		attrList.List(),
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(hPC),
		unsafe.Sizeof(hPC),
	); err != nil {
		return 0, 0, 0, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	si := &windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attrList.List(),
	}

	// Один шелл без аргументов — строка как раньше, байт в байт (путь без
	// кавычек). Аргументы разметки команд (-NoExit -EncodedCommand …) —
	// по правилам CommandLineToArgvW: путь pwsh лежит в «Program Files».
	line := argv[0]
	if len(argv) > 1 {
		line = windows.ComposeCommandLine(argv)
	}
	cmdLine, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return 0, 0, 0, err
	}
	cwdPtr, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return 0, 0, 0, err
	}

	// Build env block with npm user paths injected into PATH (service context
	// inherits System PATH, so npm global bins in %APPDATA%\npm are invisible).
	envBlock := buildEnvBlock(extraEnv)
	var envPtr *uint16
	if len(envBlock) > 0 {
		envPtr = &envBlock[0]
	}

	// CREATE_SUSPENDED: шелл сажается в job ДО первой инструкции, чтобы ни один
	// его потомок не успел родиться вне джоба (иначе гонка — ранний потомок
	// не унаследует джоб и переживёт закрытие терминала).
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(
		nil,
		cmdLine,
		nil,
		nil,
		false, // bInheritHandles
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_SUSPENDED,
		envPtr,
		cwdPtr,
		&si.StartupInfo,
		&pi,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("CreateProcessW: %w", err)
	}

	job := makeKillOnCloseJob()
	if job != 0 {
		if err := windows.AssignProcessToJobObject(job, windows.Handle(pi.Process)); err != nil {
			// Не смогли посадить в джоб (экзотика: запрет вложенных джобов до
			// Win8 и т.п.) — живём без него, как раньше.
			windows.CloseHandle(job)
			job = 0
		}
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		// Шелл так и не начал исполняться — прибираем всё, сессия не создаётся.
		if job != 0 {
			_ = windows.TerminateJobObject(job, 1)
			windows.CloseHandle(job)
		} else {
			_ = windows.TerminateProcess(windows.Handle(pi.Process), 1)
		}
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return 0, 0, 0, fmt.Errorf("ResumeThread: %w", err)
	}

	windows.CloseHandle(pi.Thread)
	return syscall.Handle(pi.Process), pi.ProcessId, job, nil
}

func updateProcThreadAttributeValue(attrList *windows.ProcThreadAttributeList, attr uintptr, value uintptr, size uintptr) error {
	r, _, err := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(attrList)),
		0,
		attr,
		value,
		size,
		0,
		0,
	)
	if r == 0 {
		if err != syscall.Errno(0) {
			return err
		}
		return syscall.EINVAL
	}
	return nil
}

func (p *conPTY) Read(buf []byte) (int, error) {
	return p.outPipe.Read(buf)
}

// readAvailable never waits for future ConPTY output. PeekNamedPipe and the
// following read have a single reader, so the reported bytes cannot disappear.
func (p *conPTY) readAvailable(buf []byte) (int, error) {
	var available uint32
	ok, _, callErr := procPeekNamedPipe.Call(
		p.outPipe.Fd(),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&available)),
		0,
	)
	if ok == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, syscall.EPIPE
	}
	if available == 0 || len(buf) == 0 {
		return 0, nil
	}
	if uint32(len(buf)) > available {
		buf = buf[:available]
	}
	return p.outPipe.Read(buf)
}

func (p *conPTY) Write(data []byte) (int, error) {
	return p.inPipe.Write(data)
}

func (p *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(windows.Handle(p.hPC), windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (p *conPTY) Close() error {
	windows.ClosePseudoConsole(windows.Handle(p.hPC))
	p.inPipe.Close()
	p.outPipe.Close()
	// Убиваем ВСЁ дерево шелла через job (включая detached-потомков — codex
	// и его node_repl/MCP-серверы), а не только корневой процесс.
	if p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 0)
		windows.CloseHandle(p.job)
	}
	// Fallback + подстраховка, если джоба нет.
	syscall.TerminateProcess(p.procHandle, 0)
	syscall.CloseHandle(p.procHandle)
	return nil
}

// buildEnvBlock returns a UTF-16 environment block inheriting the current
// process env with npm user-profile paths prepended to PATH and user-profile
// vars pointed at the first user found under C:\Users.
//
// Under LocalSystem service context USERPROFILE is
// C:\Windows\System32\config\systemprofile — apps that store state under
// %USERPROFILE%/%HOME% (Claude Code's ~/.claude, git, npm, ssh) then can't
// see the real user's data. When running as a service we redirect those
// variables to the first real user found under C:\Users so a PTY shell
// launched from APK behaves exactly like a terminal opened locally.
//
// Format: concatenated "KEY=VAL\0" UTF-16 strings terminated by an extra \0.
//
// extraEnv (разметка команд, ST-10) ложится последним и перекрывает
// одноимённые переменные без учёта регистра, как это делает сама Windows.
func buildEnvBlock(extraEnv []string) []uint16 {
	env := os.Environ()

	var npmPaths []string
	var firstUser string
	if entries, err := os.ReadDir(`C:\Users`); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			switch name {
			case "Public", "Default", "Default User", "All Users":
				continue
			}
			userDir := filepath.Join(`C:\Users`, name)
			npmDir := filepath.Join(userDir, "AppData", "Roaming", "npm")
			if info, err := os.Stat(npmDir); err == nil && info.IsDir() {
				npmPaths = append(npmPaths, npmDir)
				if firstUser == "" {
					firstUser = userDir
				}
			}
		}
	}

	currentUserProfile := strings.ToLower(os.Getenv("USERPROFILE"))
	inService := currentUserProfile == "" ||
		strings.Contains(currentUserProfile, `\systemprofile`) ||
		strings.HasPrefix(currentUserProfile, `c:\windows\`)
	redirect := inService && firstUser != ""

	var appData, localAppData, userName, homeDrive, homePath string
	if firstUser != "" {
		appData = filepath.Join(firstUser, "AppData", "Roaming")
		localAppData = filepath.Join(firstUser, "AppData", "Local")
		userName = filepath.Base(firstUser)
		if len(firstUser) >= 2 {
			homeDrive = firstUser[:2]
			homePath = firstUser[2:]
		}
	}

	has := map[string]bool{}
	for i, v := range env {
		eq := strings.Index(v, "=")
		if eq < 0 {
			continue
		}
		key := strings.ToUpper(v[:eq])
		has[key] = true
		switch key {
		case "PATH":
			if len(npmPaths) > 0 {
				env[i] = "PATH=" + strings.Join(npmPaths, ";") + ";" + v[eq+1:]
			}
		case "USERPROFILE":
			if redirect {
				env[i] = "USERPROFILE=" + firstUser
			}
		case "HOME":
			if redirect {
				env[i] = "HOME=" + firstUser
			}
		case "APPDATA":
			if redirect {
				env[i] = "APPDATA=" + appData
			}
		case "LOCALAPPDATA":
			if redirect {
				env[i] = "LOCALAPPDATA=" + localAppData
			}
		case "HOMEDRIVE":
			if redirect && homeDrive != "" {
				env[i] = "HOMEDRIVE=" + homeDrive
			}
		case "HOMEPATH":
			if redirect && homePath != "" {
				env[i] = "HOMEPATH=" + homePath
			}
		case "USERNAME":
			if redirect {
				env[i] = "USERNAME=" + userName
			}
		}
	}
	if !has["PATH"] && len(npmPaths) > 0 {
		env = append(env, "PATH="+strings.Join(npmPaths, ";"))
	}
	if firstUser != "" {
		if !has["USERPROFILE"] {
			env = append(env, "USERPROFILE="+firstUser)
		}
		if !has["APPDATA"] {
			env = append(env, "APPDATA="+appData)
		}
		if !has["LOCALAPPDATA"] {
			env = append(env, "LOCALAPPDATA="+localAppData)
		}
		if !has["HOME"] {
			env = append(env, "HOME="+firstUser)
		}
		if !has["HOMEDRIVE"] && homeDrive != "" {
			env = append(env, "HOMEDRIVE="+homeDrive)
		}
		if !has["HOMEPATH"] && homePath != "" {
			env = append(env, "HOMEPATH="+homePath)
		}
	}

	// Tools written in Rust/Go (Codex, gh, gemini) use Win32
	// SHGetKnownFolderPath() instead of reading %USERPROFILE%, so under
	// LocalSystem they resolve to systemprofile despite our redirect above.
	// Set their explicit override env vars so they pick up the real user's
	// auth/config directory.
	if redirect {
		if !has["CODEX_HOME"] {
			env = append(env, "CODEX_HOME="+filepath.Join(firstUser, ".codex"))
		}
		if !has["GH_CONFIG_DIR"] {
			env = append(env, "GH_CONFIG_DIR="+filepath.Join(firstUser, "AppData", "Roaming", "GitHub CLI"))
		}
		if !has["GEMINI_HOME"] {
			env = append(env, "GEMINI_HOME="+filepath.Join(firstUser, ".gemini"))
		}
		if !has["XDG_CONFIG_HOME"] {
			env = append(env, "XDG_CONFIG_HOME="+filepath.Join(firstUser, ".config"))
		}
	}

	env = mergeEnv(env, extraEnv, true)

	var block []uint16
	for _, v := range env {
		u, err := syscall.UTF16FromString(v)
		if err != nil {
			continue
		}
		block = append(block, u...) // includes trailing \0
	}
	block = append(block, 0) // final \0 terminates the block
	return block
}

func defaultShell() string {
	// Resolve full path — service context may have restricted PATH.
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
}
