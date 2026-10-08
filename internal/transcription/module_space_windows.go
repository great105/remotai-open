package transcription

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func checkModuleSpace(path string, needed uint64) error {
	directory, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(directory, &available, nil, nil); err != nil {
		return err
	}
	if available < needed {
		return fmt.Errorf("недостаточно места на диске компьютера: освободите не менее %.1f ГБ и повторите установку", float64(needed)/1e9)
	}
	return nil
}
