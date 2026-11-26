package connect

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if err := checkProfileDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return "", errors.New("Murmur credential file must be private (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return "", err
	}
	if len(contents) == 0 || len(contents) == 4096 {
		return "", errors.New("invalid saved Murmur credential")
	}
	plaintext, err := decodeProfile(contents)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(plaintext))
	if key == "" || len(key) >= 1024 || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("invalid saved Murmur credential")
	}
	return key, nil
}

func saveKey(path, key string) error {
	contents, err := encodeProfile([]byte(key))
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".enrollment-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(contents); err != nil {
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
	if allowEnroll && admin == "" && o.enrollmentURL == "" && o.discoverEnrollment {
		var capabilities struct {
			PrivateEnrollment bool `json:"private_enrollment"`
		}
		if err := c.request(ctx, http.MethodGet, "/health", "", nil, &capabilities); err != nil {
			return "", fmt.Errorf("check private enrollment availability: %w", err)
		}
		if capabilities.PrivateEnrollment {
			o.enrollmentURL = "http://murmur:4445"
		}
	}
	if !allowEnroll || (admin == "" && o.enrollmentURL == "") {
		return "", fmt.Errorf("no saved credential for %q; enroll from a trusted launcher or supply its MURMUR_KEY", o.agent)
	}
	unlock, err := prepareProfile(path)
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
	if o.enrollmentURL != "" {
		return enrollOnDocker(ctx, c, o, path)
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

func importCredential(ctx context.Context, c *client, o options, key string) error {
	if err := c.verify(ctx, o.agent, key); err != nil {
		return err
	}
	path, err := profilePath(o.dir, c.base, o.agent)
	if err != nil {
		return err
	}
	unlock, err := prepareProfile(path)
	if err != nil {
		return err
	}
	defer unlock()
	return saveKey(path, key)
}

func checkProfileDir(path string) error {
	dir, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !dir.IsDir() || (runtime.GOOS != "windows" && dir.Mode().Perm()&0077 != 0) {
		return errors.New("Murmur credential directory must be private (0700)")
	}
	return nil
}

func prepareProfile(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := checkProfileDir(dir); err != nil {
		return nil, err
	}
	return lockProfile(path + ".lock")
}
