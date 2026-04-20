/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package vault provides a lightweight Vault client for the WaldurMachine controller.
// It uses plain net/http with Vault's REST API — no external Vault SDK required.
//
// The controller authenticates to Vault via the Kubernetes auth method: the pod's
// ServiceAccount token is exchanged for a short-lived Vault token at startup. The
// token is stored in memory and refreshed on 401/403 responses.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const defaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Client defines the Vault operations required by the WaldurMachine controller.
type Client interface {
	// SecretExists reports whether a KV v2 secret exists at the given logical path.
	SecretExists(ctx context.Context, path string) (bool, error)
	// WriteSecret writes key-value pairs to a KV v2 secret at the given logical path.
	WriteSecret(ctx context.Context, path string, data map[string]string) error
	// GenerateSecretID generates a single-use AppRole secret_id for the given role name.
	GenerateSecretID(ctx context.Context, roleName string) (string, error)
	// RevokeSecretID destroys an unused AppRole secret_id (cleanup on VM creation failure).
	RevokeSecretID(ctx context.Context, roleName, secretID string) error
}

// Config holds the configuration for connecting to Vault.
type Config struct {
	// Addr is the Vault server address, e.g. "https://vault.example.com:8200".
	Addr string
	// AuthPath is the Vault Kubernetes auth mount path (default: "auth/kubernetes").
	AuthPath string
	// Role is the Vault Kubernetes auth role name for the controller.
	Role string
	// TokenPath is the path to the pod ServiceAccount JWT token file.
	// Defaults to /var/run/secrets/kubernetes.io/serviceaccount/token.
	TokenPath string
}

type client struct {
	cfg        Config
	httpClient *http.Client
	vaultToken string
}

// NewClient creates a Vault client and authenticates using the Kubernetes auth method.
func NewClient(cfg Config) (Client, error) {
	if cfg.AuthPath == "" {
		cfg.AuthPath = "auth/kubernetes"
	}
	if cfg.TokenPath == "" {
		cfg.TokenPath = defaultTokenPath
	}

	c := &client{
		cfg:        cfg,
		httpClient: &http.Client{},
	}

	if err := c.login(); err != nil {
		return nil, fmt.Errorf("vault: kubernetes auth login failed: %w", err)
	}
	return c, nil
}

// login exchanges the pod ServiceAccount JWT for a Vault token.
func (c *client) login() error {
	jwt, err := os.ReadFile(c.cfg.TokenPath)
	if err != nil {
		return fmt.Errorf("vault: unable to read service account token: %w", err)
	}

	body, _ := json.Marshal(map[string]string{
		"role": c.cfg.Role,
		"jwt":  string(jwt),
	})

	url := fmt.Sprintf("%s/v1/%s/login", c.cfg.Addr, c.cfg.AuthPath)
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("vault: kubernetes auth request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vault: kubernetes auth returned %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("vault: failed to decode auth response: %w", err)
	}
	if result.Auth.ClientToken == "" {
		return fmt.Errorf("vault: kubernetes auth returned empty token")
	}
	c.vaultToken = result.Auth.ClientToken
	return nil
}

// do performs an authenticated request, retrying once after re-login on 401/403.
func (c *client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	resp, err := c.doOnce(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close()
		if loginErr := c.login(); loginErr != nil {
			return nil, fmt.Errorf("vault: re-authentication failed: %w", loginErr)
		}
		return c.doOnce(ctx, method, path, body)
	}
	return resp, nil
}

func (c *client) doOnce(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("vault: failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	url := fmt.Sprintf("%s/v1/%s", c.cfg.Addr, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", c.vaultToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}

// SecretExists reports whether a KV v2 secret exists at the given logical path.
// path should be in the form "secret/data/..." — the raw KV v2 API path.
func (c *client) SecretExists(ctx context.Context, path string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, fmt.Errorf("vault: SecretExists GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("vault: SecretExists returned %d: %s", resp.StatusCode, string(b))
	}
	return true, nil
}

// WriteSecret writes key-value pairs to a KV v2 secret.
// path should be in the form "secret/data/..." — the raw KV v2 API path.
func (c *client) WriteSecret(ctx context.Context, path string, data map[string]string) error {
	payload := map[string]any{
		"data": data,
	}
	resp, err := c.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return fmt.Errorf("vault: WriteSecret POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vault: WriteSecret returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// GenerateSecretID generates a single-use AppRole secret_id for the given role name.
func (c *client) GenerateSecretID(ctx context.Context, roleName string) (string, error) {
	path := fmt.Sprintf("auth/approle/role/%s/secret-id", roleName)
	resp, err := c.do(ctx, http.MethodPost, path, map[string]any{})
	if err != nil {
		return "", fmt.Errorf("vault: GenerateSecretID for role %q: %w", roleName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vault: GenerateSecretID returned %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("vault: failed to decode secret_id response: %w", err)
	}
	if result.Data.SecretID == "" {
		return "", fmt.Errorf("vault: empty secret_id in response for role %q", roleName)
	}
	return result.Data.SecretID, nil
}

// RevokeSecretID destroys an unused AppRole secret_id. Called on VM creation failure
// to prevent a dangling single-use credential from sitting in Vault until TTL expiry.
func (c *client) RevokeSecretID(ctx context.Context, roleName, secretID string) error {
	path := fmt.Sprintf("auth/approle/role/%s/secret-id/destroy", roleName)
	resp, err := c.do(ctx, http.MethodPost, path, map[string]string{"secret_id": secretID})
	if err != nil {
		return fmt.Errorf("vault: RevokeSecretID for role %q: %w", roleName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vault: RevokeSecretID returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
