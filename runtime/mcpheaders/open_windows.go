//go:build windows

package mcpheaders

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

var openBearerFile = openBearerFileNoFollow

func openBearerFileNoFollow(path string) (*os.File, os.FileInfo, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, nil, syscall.ELOOP
	}
	return file, info, nil
}
