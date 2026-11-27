package connect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// discoverPrivateEnrollment asks the bus whether its private enrollment
// listener is serving and returns its URL on the bus host. A degraded memory
// store makes /health return 503, which does not affect enrollment, so the
// body is read for either status.
func discoverPrivateEnrollment(ctx context.Context, c *client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return "", &statusError{path: req.URL.Path, code: resp.StatusCode}
	}
	var health struct {
		PrivateEnrollment bool `json:"private_enrollment"`
		Port              int  `json:"private_enrollment_port"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&health); err != nil {
		return "", err
	}
	if !health.PrivateEnrollment {
		return "", nil
	}
	port := health.Port
	if port == 0 {
		port = 4445 // servers that predate the advertised port
	}
	if port < 1 || port > 65535 {
		return "", errors.New("bus advertised an invalid private enrollment port")
	}
	bus, err := url.Parse(c.base)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(bus.Hostname(), strconv.Itoa(port)), nil
}

var errClaimRejected = errors.New("private enrollment refused the claim")

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
			// A 409 is final: a lost response to a claim that succeeded
			// comes back 200 on retry. Drop the pending key so a later
			// admin enrollment of this profile is not blocked by it.
			_ = os.Remove(pending)
			return "", fmt.Errorf("%w: agent name %q is already reserved; use its saved profile or ask an administrator to enroll it", errClaimRejected, o.agent)
		}
		var netErr net.Error
		if status == nil && errors.As(err, &netErr) && ctx.Err() == nil {
			return "", fmt.Errorf("the bus advertises private enrollment at %s, but it is unreachable from here; join the Murmur Compose network or supply MURMUR_KEY: %w", enrollment.base, err)
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
