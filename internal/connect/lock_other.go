//go:build !unix

package connect

import "os"

func lockProfile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return func() {
		_ = file.Close()
		_ = os.Remove(path)
	}, nil
}
