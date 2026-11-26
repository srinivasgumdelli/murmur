package connect

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func profilePath(dir, base, agent string) (string, error) {
	if dir == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(root, "murmur", "credentials")
	}
	sum := sha256.Sum256([]byte(base + "\x00" + agent))
	return filepath.Join(dir, fmt.Sprintf("%x.key", sum)), nil
}

func readKey(path string) (string, error) {
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return "", errors.New("Murmur credential directory must be private (0700)")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("Murmur credential file must be private (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, 1024))
	if err != nil {
		return "", err
	}
	if len(contents) == 0 || len(contents) == 1024 {
		return "", errors.New("invalid saved Murmur credential")
	}
	return string(contents), nil
}

func saveKey(path, key string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".enrollment-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(key); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func loadCredential(ctx context.Context, c *client, o options, allowEnroll bool) (string, error) {
	if key := os.Getenv("MURMUR_KEY"); key != "" {
		return key, c.verify(ctx, o.agent, key)
	}
	path, err := profilePath(o.dir, c.base, o.agent)
	if err != nil {
		return "", err
	}
	key, err := readKey(path)
	if err == nil {
		return key, c.verify(ctx, o.agent, key)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	admin := os.Getenv("MURMUR_ADMIN_KEY")
	if !allowEnroll || admin == "" {
		return "", fmt.Errorf("no saved credential for %q; enroll from a trusted launcher or supply its MURMUR_KEY", o.agent)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return "", errors.New("Murmur credential directory must be private (0700)")
	}
	unlock, err := lockProfile(path + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	key, err = readKey(path)
	if err == nil {
		return key, c.verify(ctx, o.agent, key)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key, err = c.enroll(ctx, o.agent, admin)
	if err != nil {
		return "", err
	}
	if err = c.verify(ctx, o.agent, key); err != nil {
		return "", err
	}
	return key, saveKey(path, key)
}
