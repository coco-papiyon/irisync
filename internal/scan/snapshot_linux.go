package scan

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

func entryChangeStamp(_ *os.File, info fs.FileInfo) ([2]int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int64{}, errors.New("file status change time is unavailable")
	}
	return [2]int64{int64(stat.Ctim.Sec), int64(stat.Ctim.Nsec)}, nil
}
