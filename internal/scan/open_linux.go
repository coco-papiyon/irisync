package scan

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func openRoot(absolute string) (*os.File, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, absolute, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, openError(err)
	}
	return os.NewFile(uintptr(fd), absolute), nil
}

func openRelative(root *os.File, relative string) (*os.File, error) {
	// O_PATH obtains an identity without opening a device, pipe or socket
	// for I/O. The kernel checks the entire lookup against the root.
	fd, err := unix.Openat2(int(root.Fd()), relative, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, openError(err)
	}
	probe := os.NewFile(uintptr(fd), relative)
	defer probe.Close()
	info, err := probe.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, errExcluded
	}

	// Reopen the kernel's descriptor for the exact object already checked,
	// without resolving its name again in the synchronization tree.
	opened, err := unix.Open("/proc/self/fd/"+strconv.Itoa(fd),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open checked descriptor through procfs: %w", err)
	}
	file := os.NewFile(uintptr(opened), relative)
	reopened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !os.SameFile(info, reopened) {
		file.Close()
		return nil, errors.New("descriptor no longer identifies the checked entry")
	}
	return file, nil
}

func openError(err error) error {
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV):
		return errExcluded
	case errors.Is(err, unix.ENOSYS):
		return fmt.Errorf("safe path lookup requires Linux openat2: %w", err)
	default:
		return err
	}
}
