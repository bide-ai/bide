//go:build windows

package filegate

import (
	"fmt"
	"syscall"
)

// identity is the volume serial number and file index of the file at path.
func identity(path string) (string, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", false
	}
	defer syscall.CloseHandle(h)
	var fi syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &fi); err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d", fi.VolumeSerialNumber, fi.FileIndexHigh, fi.FileIndexLow), true
}
