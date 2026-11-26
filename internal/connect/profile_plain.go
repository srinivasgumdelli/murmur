//go:build !windows

package connect

func encodeProfile(data []byte) ([]byte, error) { return data, nil }
func decodeProfile(data []byte) ([]byte, error) { return data, nil }
