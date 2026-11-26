package connect

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func lockProfile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var offset windows.Overlapped
	// The handle holds the lock, so process exit releases it even after a crash.
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &offset); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}

func encodeProfile(data []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(data)), Data: unsafe.SliceData(data)}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	return copyDataBlob(output), nil
}

func decodeProfile(data []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(data)), Data: unsafe.SliceData(data)}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	return copyDataBlob(output), nil
}

func copyDataBlob(blob windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data))))
	return append([]byte(nil), unsafe.Slice(blob.Data, blob.Size)...)
}
