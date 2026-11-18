//go:build darwin && cgo

package main

/*
#include <string.h>
#include <errno.h>
#include <sys/attr.h>
#include <unistd.h>
#include <fcntl.h>

// dc_openat opens a subdirectory relative to its parent, never following
// symlinks. Retries EINTR: opens inside sandbox containers can block on a
// policy check long enough to be interrupted by a signal.
static int dc_openat(int dirfd, const char *name) {
	int fd;
	do {
		fd = openat(dirfd, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
	} while (fd < 0 && errno == EINTR);
	return fd < 0 ? -errno : fd;
}

// dc_bulk reads the next batch of directory entries with their attributes in
// one system call. Returns the number of entries, 0 at the end, or -errno.
static int dc_bulk(int fd, void *buf, size_t size, unsigned int cmnattr, unsigned int dirattr, unsigned int fileattr) {
	struct attrlist al;
	memset(&al, 0, sizeof(al));
	al.bitmapcount = ATTR_BIT_MAP_COUNT;
	// No ATTR_CMN_OBJTYPE on purpose: asking for it makes APFS report the
	// directory underneath a graft (MobileAsset .AssetData) and firmlink stubs
	// instead of what lstat sees. The type is implied by which group (dir or
	// file attributes) comes back.
	al.commonattr = ATTR_CMN_RETURNED_ATTRS | cmnattr;
	al.dirattr = dirattr;
	al.fileattr = fileattr;
	int n;
	do {
		n = getattrlistbulk(fd, &al, buf, size, 0);
	} while (n < 0 && errno == EINTR);
	return n < 0 ? -errno : n;
}
*/
import "C"

import (
	"encoding/binary"
	"os"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

const (
	attrCmnName       = 0x00000001
	attrCmnDevID      = 0x00000002
	attrCmnModTime    = 0x00000400
	attrCmnFileID     = 0x02000000
	attrCmnError      = 0x20000000
	attrDirMountStat  = 0x00000004
	attrDirAllocSize  = 0x00000008
	attrFileLinkCount = 0x00000001
	attrFileAllocSize = 0x00000004
	dirMntPoint       = 0x00000001 // DIR_MNTSTATUS_MNTPOINT in <sys/attr.h>
)

var (
	bulkBufSize   = 256 << 10 // first call per directory; enough for ~2000 entries
	bigBufSize    = 8 << 20   // rest of a large directory, in as few calls as possible
	bulkCmnAttrs  = uint32(attrCmnName | attrCmnDevID | attrCmnModTime | attrCmnFileID | attrCmnError)
	bulkDirAttrs  = uint32(attrDirMountStat | attrDirAllocSize)
	bulkFileAttrs = uint32(attrFileLinkCount | attrFileAllocSize)

	// Each getattrlistbulk call on APFS re-seeks to its position in the
	// directory, so huge folders (browser caches with 80k files) get much
	// faster when the remainder is read with one big buffer instead of many
	// small ones. Pooled so only workers that hit such folders hold one.
	bigBufPool = sync.Pool{New: func() any { b := make([]byte, bigBufSize); return &b }}
)

// openDirAt opens parent/name relative to the parent's descriptor, so the
// kernel resolves one path component instead of the whole path.
func openDirAt(parent int, name, _ string) (int, error) {
	var stack [256]byte
	buf := stack[:0]
	if len(name) >= len(stack) {
		buf = make([]byte, 0, len(name)+1)
	}
	buf = append(append(buf, name...), 0)
	fd := int(C.dc_openat(C.int(parent), (*C.char)(unsafe.Pointer(&buf[0]))))
	if fd < 0 {
		return -1, syscall.Errno(-fd)
	}
	return fd, nil
}

// statDir reads the mtime and allocated size of an open directory.
func statDir(fd int) (mod, size int64, ok bool) {
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return 0, 0, false
	}
	return st.Mtimespec.Sec, st.Blocks * 512, true
}

func (s *Scan) walkerName() string {
	if s.portable {
		return "portable"
	}
	return "getattrlistbulk"
}

func (s *Scan) readDir(fd int, path string, b *dirBuf) error {
	if s.portable {
		return s.readDirPortable(path, b)
	}
	return s.readDirBulk(fd, path, b)
}

// readDirBulk lists a directory with getattrlistbulk(2): names, types, sizes,
// mtimes and inode numbers for many entries per system call, instead of one
// lstat per file. Symlinks are reported, never followed.
func (s *Scan) readDirBulk(fd int, path string, b *dirBuf) error {
	if len(b.io) != bulkBufSize {
		b.io = make([]byte, bulkBufSize)
	}
	buf := b.io
	var big *[]byte
	defer func() {
		if big != nil {
			bigBufPool.Put(big)
		}
	}()
	le := binary.LittleEndian
	for {
		n := int(C.dc_bulk(C.int(fd), unsafe.Pointer(&buf[0]), C.size_t(len(buf)), C.uint(bulkCmnAttrs), C.uint(bulkDirAttrs), C.uint(bulkFileAttrs)))
		if n < 0 {
			return &os.PathError{Op: "getattrlistbulk", Path: path, Err: syscall.Errno(-n)}
		}
		if n == 0 {
			return nil
		}
		off := 0
		for i := 0; i < n; i++ {
			rec := buf[off:]
			reclen := int(le.Uint32(rec))
			if reclen <= 0 || off+reclen > len(buf) {
				return &os.PathError{Op: "getattrlistbulk", Path: path, Err: syscall.EIO}
			}
			off += reclen
			rec = rec[:reclen]

			// Layout: length, attribute_set_t returned, [error], then the
			// returned attributes in bit order: common, dir, file.
			common, dirA, fileA := le.Uint32(rec[4:]), le.Uint32(rec[12:]), le.Uint32(rec[16:])
			p := 24
			var errno uint32
			if common&attrCmnError != 0 {
				errno = le.Uint32(rec[p:])
				p += 4
			}
			var name []byte
			if common&attrCmnName != 0 {
				start := p + int(int32(le.Uint32(rec[p:]))) // offset is relative to the attrreference
				l := int(le.Uint32(rec[p+4:]))              // includes the trailing NUL
				if l > 0 && start >= 0 && start+l <= reclen {
					name = rec[start : start+l-1]
				}
				p += 8
			}
			var e rawEntry
			if common&attrCmnDevID != 0 {
				e.dev = uint64(le.Uint32(rec[p:]))
				p += 4
			}
			if common&attrCmnModTime != 0 {
				e.mod = int64(le.Uint64(rec[p:])) // struct timespec: tv_sec, tv_nsec
				p += 16
			}
			if common&attrCmnFileID != 0 {
				e.ino = le.Uint64(rec[p:])
				p += 8
			}
			if dirA&attrDirMountStat != 0 {
				// A mount point reports the covered directory; lstat (and the
				// portable walker) report the mounted volume's root instead.
				if le.Uint32(rec[p:])&dirMntPoint != 0 {
					e.restat = true
				}
				p += 4
			}
			if dirA&attrDirAllocSize != 0 {
				e.size = int64(le.Uint64(rec[p:]))
				p += 8
			}
			if fileA&attrFileLinkCount != 0 {
				e.nlink = le.Uint32(rec[p:])
				p += 4
			}
			if fileA&attrFileAllocSize != 0 {
				e.size = int64(le.Uint64(rec[p:]))
			}

			if errno != 0 || name == nil {
				if errno != 0 && syscall.Errno(errno) != syscall.ENOENT {
					cp := joinPath(path, string(name))
					s.recordErr(cp, &os.PathError{Op: "getattrlistbulk", Path: cp, Err: syscall.Errno(errno)})
				}
				continue
			}
			switch {
			case dirA != 0:
				e.dir = true
			case fileA != 0: // regular file, symlink, fifo, socket…
			default: // file system returned neither group: ask lstat
				var st syscall.Stat_t
				if err := syscall.Lstat(joinPath(path, string(name)), &st); err != nil {
					continue
				}
				e.dir = st.Mode&syscall.S_IFMT == syscall.S_IFDIR
				e.size, e.mod, e.nlink = st.Blocks*512, st.Mtimespec.Sec, uint32(st.Nlink)
				e.dev, e.ino = uint64(st.Dev), st.Ino
			}
			b.add(name, e)
		}
		if big == nil && off > len(buf)/2 { // probably more to come: switch to the big buffer
			big = bigBufPool.Get().(*[]byte)
			buf = *big
		}
	}
}

func setBenchTuning() {
	if n, _ := strconv.Atoi(os.Getenv("DC_BULKBUF")); n >= 4096 {
		bulkBufSize = n
	}
	hex := func(k string, v *uint32) {
		if n, err := strconv.ParseUint(os.Getenv(k), 0, 32); err == nil {
			*v = uint32(n)
		}
	}
	hex("DC_CMN", &bulkCmnAttrs)
	hex("DC_DIRA", &bulkDirAttrs)
	hex("DC_FILEA", &bulkFileAttrs)
}
