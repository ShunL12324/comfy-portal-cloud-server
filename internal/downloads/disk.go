package downloads

import (
	"errors"
	"syscall"
)

func freeBytes(path string) (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	// Bsize is signed on Linux and unsigned on macOS; go through int64 so one
	// guard covers both.
	blockSize := int64(s.Bsize)
	if blockSize <= 0 {
		return 0, errors.New("statfs reported a non-positive block size")
	}
	return uint64(s.Bavail) * uint64(blockSize), nil
}
