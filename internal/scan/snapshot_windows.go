package scan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The padding keeps the layout of Win32 FILE_BASIC_INFO on 32-bit builds.
type fileBasicInfo struct {
	creationTime, lastAccessTime, lastWriteTime, changeTime int64
	attributes                                              uint32
	_                                                       uint32
}

func entryChangeStamp(file *os.File, _ fs.FileInfo) ([2]int64, error) {
	var info fileBasicInfo
	err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		return [2]int64{}, fmt.Errorf("read file status change time: %w", err)
	}
	if info.changeTime == 0 {
		return [2]int64{}, errors.New("file status change time is unavailable")
	}
	return [2]int64{info.changeTime}, nil
}
