package hermes

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

// NtCreateFile resolves a SINGLE component relative to the pinned parent
// handle. Reparse points (including junctions) are opened as objects, never
// followed, and rejected by their handle attributes before any data is read.
func openArtifactNoFollow(root, rel string, beforeOpen func(string)) (*os.File, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, errors.New("artifact requires a local disk")
	}
	name, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return nil, err
	}
	parent, err := windows.CreateFile(name, windows.FILE_GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	parts := append(strings.Split(strings.TrimLeft(abs[len(volume):], `\/`), `\`), strings.Split(rel, string(filepath.Separator))...)
	for i, part := range parts {
		if part == "" {
			continue
		}
		if beforeOpen != nil {
			beforeOpen(part)
		}
		object, e := windows.NewNTUnicodeString(part)
		if e != nil {
			windows.CloseHandle(parent)
			return nil, e
		}
		oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: object, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
		options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
		if i < len(parts)-1 {
			options |= windows.FILE_DIRECTORY_FILE
		} else {
			options |= windows.FILE_NON_DIRECTORY_FILE
		}
		var next windows.Handle
		var status windows.IO_STATUS_BLOCK
		e = windows.NtCreateFile(&next, windows.FILE_GENERIC_READ, &oa, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0)
		windows.CloseHandle(parent)
		if e != nil {
			return nil, e
		}
		var info windows.ByHandleFileInformation
		e = windows.GetFileInformationByHandle(next, &info)
		if e != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			windows.CloseHandle(next)
			return nil, errors.New("artifact reparse point refused")
		}
		parent = next
	}
	return os.NewFile(uintptr(parent), filepath.Join(abs, rel)), nil
}
