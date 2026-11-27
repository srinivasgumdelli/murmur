package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/model"
)

type client struct {
	base string
	http *http.Client
}

func newClient(raw string) (*client, error) {
	target, err := url.Parse(raw)
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" ||
		target.ForceQuery || target.Fragment != "" || target.Opaque != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		return nil, errors.New("Murmur URL must be an http or https base URL")
	}
	target.Path = strings.TrimRight(target.Path, "/")
	target.RawPath = strings.TrimRight(target.RawPath, "/")
	return &client{base: target.String(), http: &http.Client{
		Timeout: 40 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

type statusError struct {
	path    string
	code    int
	message string // first line of the server's error text, if any
}

func (e *statusError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("Murmur %s returned HTTP %d: %s", e.path, e.code, e.message)
	}
	return fmt.Sprintf("Murmur %s returned HTTP %d", e.path, e.code)
}

func (c *client) request(ctx context.Context, method, path, key string, body, out any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("X-Murmur-Key", key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message, _, _ := strings.Cut(strings.TrimSpace(string(text)), "\n")
		return &statusError{path: req.URL.Path, code: resp.StatusCode, message: message}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func (c *client) enroll(ctx context.Context, agent, adminKey string) (string, error) {
	var issuer model.Identity
	if err := c.request(ctx, http.MethodGet, "/identity", adminKey, nil, &issuer); err != nil {
		return "", fmt.Errorf("verify enrollment administrator: %w", err)
	}
	if !issuer.Admin {
		return "", errors.New("the enrollment credential is not a Murmur administrator")
	}
	var result struct {
		Agent string `json:"agent"`
		Key   string `json:"key"`
	}
	err := c.request(ctx, http.MethodPost, "/keys", adminKey, map[string]any{
		"agent": agent, "enroll": true,
	}, &result)
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusConflict && strings.Contains(status.message, "letter case") {
		return "", fmt.Errorf("enroll %s: %s", agent, status.message)
	}
	if errors.As(err, &status) && status.code == http.StatusConflict {
		return "", fmt.Errorf("agent %q claimed its name through private enrollment. If that was your agent, use its saved profile. Otherwise revoke its keys with POST /keys {\"agent\":%q,\"enroll\":true,\"reclaim\":true}", agent, agent)
	}
	if err != nil {
		return "", fmt.Errorf("enroll %s: %w", agent, err)
	}
	if result.Agent != agent || result.Key == "" {
		return "", errors.New("the server returned an incomplete enrollment")
	}
	return result.Key, nil
}

func (c *client) verify(ctx context.Context, agent, key string) error {
	var identity model.Identity
	if err := c.request(ctx, http.MethodGet, "/identity", key, nil, &identity); err != nil {
		return fmt.Errorf("verify agent credential: %w", err)
	}
	if identity.Admin || identity.Agent != agent {
		return fmt.Errorf("credential does not belong to agent %q", agent)
	}
	return nil
}

func (c *client) heartbeat(ctx context.Context, agent, key string) error {
	var result model.Agent
	return c.request(ctx, http.MethodPost, "/agents/"+url.PathEscape(agent)+"/heartbeat?inbox=false", key, nil, &result)
}
