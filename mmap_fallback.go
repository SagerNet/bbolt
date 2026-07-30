package bbolt

import (
	"fmt"
	"io"
	"unsafe"
)

// mmapFallback provides a heap-backed mirror of the data file as a substitute
// for mmap on file systems that do not support memory mapping (e.g. jffs2).
func mmapFallback(db *DB, sz int) error {
	b := make([]byte, sz)

	info, err := db.file.Stat()
	if err != nil {
		return fmt.Errorf("file stat: %w", err)
	}

	readSize := int64(sz)
	if info.Size() < readSize {
		readSize = info.Size()
	}
	if readSize > 0 {
		if _, err := db.file.ReadAt(b[:readSize], 0); err != nil && err != io.EOF {
			return fmt.Errorf("file read: %w", err)
		}
	}

	db.dataref = b
	db.data = (*[maxMapSize]byte)(unsafe.Pointer(&b[0]))
	db.datasz = sz
	db.mmapFallback = true
	return nil
}

func munmapFallback(db *DB) error {
	db.dataref = nil
	db.data = nil
	db.datasz = 0
	db.mmapFallback = false
	return nil
}

// copyToMmapFallback copies written bytes into the heap mirror so the in-memory
// page view stays consistent with the file in fallback mode.
func (db *DB) copyToMmapFallback(b []byte, off int64) {
	if off < 0 || off >= int64(len(db.dataref)) {
		return
	}
	copy(db.dataref[off:], b)
}
