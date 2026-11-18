//go:build !darwin || !cgo

package main

import "syscall"

func (s *Scan) walkerName() string { return "portable" }

func openDirAt(_ int, _ string, path string) (int, error) {
	for {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

// statDir is only needed for darwin mount points reported by
// getattrlistbulk; the portable walker never asks for it.
func statDir(int) (mod, size int64, ok bool) { return 0, 0, false }

func (s *Scan) readDir(fd int, path string, b *dirBuf) error {
	return s.readDirPortable(path, b)
}

func setBenchTuning() {}
