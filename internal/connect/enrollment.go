package connect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func enrollOnDocker(ctx context.Context, bus *client, o options, path string) (string, error) {
	enrollment, err := newClient(o.enrollmentURL)
	if err != nil {
		return "", err
	}
	busURL, _ := url.Parse(bus.base)
	enrollmentURL, _ := url.Parse(enrollment.base)
	if !strings.EqualFold(busURL.Hostname(), enrollmentURL.Hostname()) || bus.base == enrollment.base {
		return "", errors.New("private enrollment must use the same host and a separate listener")
	}

	// Keep the candidate across network and disk failures so a lost response
	// can be retried with the same claim.
	pending := path + ".pending"
	key, err := readKey(pending)
	if errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return "", err
		}
		key = "mmr_" + hex.EncodeToString(secret[:])
		if err := saveKey(pending, key); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}

	digest := sha256.Sum256([]byte(key))
	var answer struct {
		Agent string `json:"agent"`
	}
	if err := enrollment.request(ctx, http.MethodPost, "/enroll", "", map[string]string{
		"agent": o.agent, "key_hash": hex.EncodeToString(digest[:]),
	}, &answer); err != nil {
		var status *statusError
		if errors.As(err, &status) && status.code == http.StatusConflict {
			return "", fmt.Errorf("agent name %q is already reserved; use its saved profile or ask an administrator to enroll it", o.agent)
		}
		return "", fmt.Errorf("private Docker enrollment: %w", err)
	}
	if answer.Agent != o.agent {
		return "", errors.New("private enrollment returned a different agent")
	}
	if err := bus.verify(ctx, o.agent, key); err != nil {
		return "", err
	}
	if err := saveKey(path, key); err != nil {
		return "", err
	}
	_ = os.Remove(pending)
	return key, nil
}
