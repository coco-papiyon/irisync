package scan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/coco-papiyon/irisync/internal/pathname"
	"golang.org/x/sys/windows"
)

func openRoot(absolute string) (*os.File, error) {
	volume := filepath.VolumeName(absolute)
	if volume == "" || !filepath.IsAbs(absolute) {
		return nil, errors.New("expected an absolute drive or UNC path")
	}
	if strings.HasPrefix(volume, "\\\\") {
		server, share, ok := strings.Cut(volume[2:], "\\")
		if !ok || !pathname.ValidWindowsName(server) || !pathname.ValidWindowsName(share) {
			return nil, errors.New("expected a standard UNC share")
		}
	} else if len(volume) != 2 || volume[1] != ':' {
		return nil, errors.New("expected a standard Windows drive")
	}

	// Open only the volume root by an absolute name. From there, the same
	// handle-relative primitive also verifies the configured root's ancestors.
	name, err := windows.UTF16PtrFromString(volume + "\\")
	if err != nil {
		return nil, err
	}
	relative := strings.TrimPrefix(absolute[len(volume):], "\\")
	access := uint32(windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE | windows.SYNCHRONIZE)
	if relative == "" {
		access = windows.FILE_GENERIC_READ
	}
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	if relative == "" {
		return checkedWindowsFile(handle, absolute, true)
	}
	defer windows.CloseHandle(handle)
	if err := requireDiskEntry(handle, true); err != nil {
		return nil, err
	}
	if err := ValidateRelativePath(filepath.ToSlash(relative)); err != nil {
		return nil, err
	}
	return openWindowsEntry(handle, relative, true)
}

func openRelative(root *os.File, relative string) (*os.File, error) {
	return openWindowsEntry(windows.Handle(root.Fd()), filepath.FromSlash(relative), false)
}

func openWindowsEntry(root windows.Handle, relative string, directory bool) (*os.File, error) {
	name, err := windows.NewNTUnicodeString(relative)
	if err != nil {
		return nil, err
	}
	attr := windows.OBJECT_ATTRIBUTES{
		RootDirectory: root,
		ObjectName:    name,
		Attributes:    windows.OBJ_DONT_REPARSE,
	}
	attr.Length = uint32(unsafe.Sizeof(attr))
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		attr.Attributes |= windows.OBJ_CASE_INSENSITIVE
		options |= windows.FILE_DIRECTORY_FILE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	// Omitting FILE_SHARE_DELETE pins the opened entry while it is being used.
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ, &attr, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN, options, 0, 0)
	if err != nil {
		if errors.Is(err, windows.STATUS_REPARSE_POINT_ENCOUNTERED) {
			return nil, errExcluded
		}
		if code, ok := err.(windows.NTStatus); ok {
			return nil, code.Errno()
		}
		return nil, err
	}
	return checkedWindowsFile(handle, relative, directory)
}

func checkedWindowsFile(handle windows.Handle, name string, directory bool) (*os.File, error) {
	if err := requireDiskEntry(handle, directory); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
}

func requireDiskEntry(handle windows.Handle, directory bool) error {
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return err
	}
	if kind != windows.FILE_TYPE_DISK {
		return errExcluded
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errExcluded
	}
	if directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("scan root is not a directory")
	}
	return nil
}
