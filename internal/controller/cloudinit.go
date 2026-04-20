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

package controller

import (
	"fmt"

	"github.com/pkg/errors"
	"go.yaml.in/yaml/v3"
)

const rke2ConfigPath = "/etc/rancher/rke2/config.yaml"

// CloudInit represents the subset of cloud-init directives used in this provider.
type CloudInit struct {
	WriteFiles []CloudInitFile `yaml:"write_files,omitempty"`
	RunCmd     []interface{}   `yaml:"runcmd,omitempty"`
	BootCmd    []interface{}   `yaml:"bootcmd,omitempty"`
}

// CloudInitFile represents a single write_files entry.
type CloudInitFile struct {
	Path        string `yaml:"path"`
	Content     string `yaml:"content"`
	Permissions string `yaml:"permissions,omitempty"`
	Owner       string `yaml:"owner,omitempty"`
	Encoding    string `yaml:"encoding,omitempty"`
}

// VaultParams holds the per-node Vault AppRole credentials injected into cloud-init.
type VaultParams struct {
	Addr       string
	SecretPath string
	RoleID     string
	SecretID   string
}

// MergeInput is the input to mergeCloudInit.
type MergeInput struct {
	// BootstrapCloudInit is the sanitised output from the RKE2 bootstrap provider
	// (token already stripped by stripRKE2Token).
	BootstrapCloudInit []byte
	// StaticCloudInit is the base OS cloud-init template (disk setup, sysctl, packages).
	// May be nil/empty — in that case only bootstrap + Vault sections are used.
	StaticCloudInit []byte
	// VaultParams contains the per-node AppRole credentials.
	VaultParams VaultParams
}

// stripRKE2Token parses the bootstrap cloud-init produced by the RKE2 bootstrap provider,
// removes the token field from the /etc/rancher/rke2/config.yaml write_files entry, and
// returns both the sanitised YAML and the extracted token value.
//
// All other content is preserved: TLS certificates, server URL, install commands,
// node labels and taints, preRKE2Commands and postRKE2Commands.
func stripRKE2Token(rawCloudInit []byte) (sanitised []byte, token string, err error) {
	var ci CloudInit
	if err := yaml.Unmarshal(rawCloudInit, &ci); err != nil {
		return nil, "", fmt.Errorf("failed to parse bootstrap cloud-init: %w", err)
	}

	for i, f := range ci.WriteFiles {
		if f.Path != rke2ConfigPath {
			continue
		}
		var rke2cfg map[string]interface{}
		if err := yaml.Unmarshal([]byte(f.Content), &rke2cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse embedded rke2 config: %w", err)
		}
		if t, ok := rke2cfg["token"].(string); ok {
			token = t
		}
		if token == "" {
			return nil, "", errors.New("token field not found in embedded rke2 config")
		}
		delete(rke2cfg, "token") // removed — injected at boot via Vault
		updated, err := yaml.Marshal(rke2cfg)
		if err != nil {
			return nil, "", err
		}
		ci.WriteFiles[i].Content = string(updated)
		break
	}

	out, err := yaml.Marshal(ci)
	return out, token, err
}

// mergeCloudInit produces the final user_data from three sources:
//
//  1. Static OS sections (prepended) — disk setup, sysctl, packages, services.
//     These must run before RKE2 installs. If StaticCloudInit is empty, this step is skipped.
//
//  2. Bootstrap sections — sanitised RKE2 bootstrap provider output: RKE2 config
//     (server URL only, no token), TLS certs, install script, systemd enable.
//
//  3. Vault additions — AppRole credential files and a token-fetch script prepended
//     as the first runcmd step, ensuring the token is in place before RKE2 starts.
func mergeCloudInit(in MergeInput) (string, error) {
	var static, bootstrap CloudInit

	if len(in.StaticCloudInit) > 0 {
		if err := yaml.Unmarshal(in.StaticCloudInit, &static); err != nil {
			return "", fmt.Errorf("failed to parse static cloud-init: %w", err)
		}
	}
	if err := yaml.Unmarshal(in.BootstrapCloudInit, &bootstrap); err != nil {
		return "", fmt.Errorf("failed to parse bootstrap cloud-init: %w", err)
	}

	merged := CloudInit{
		WriteFiles: append(static.WriteFiles, bootstrap.WriteFiles...),
		RunCmd:     append(static.RunCmd, bootstrap.RunCmd...),
		BootCmd:    append(static.BootCmd, bootstrap.BootCmd...),
	}

	// Prepend Vault credential files and the token-fetch script.
	vaultFiles := []CloudInitFile{
		{
			Path:        "/etc/vault/role-id",
			Content:     in.VaultParams.RoleID + "\n",
			Permissions: "0600",
		},
		{
			Path:        "/etc/vault/secret-id",
			Content:     in.VaultParams.SecretID + "\n",
			Permissions: "0600",
		},
		{
			Path:        "/var/opt/scripts/vault-fetch-rke2-token.sh",
			Permissions: "0755",
			Content: fmt.Sprintf(`#!/bin/bash
# Fetch the RKE2 join token from Vault using AppRole credentials.
# Runs as the first runcmd step — before RKE2 starts.
# Appends the token to /etc/rancher/rke2/config.yaml which already
# contains the server URL (written by the bootstrap provider).
set -euo pipefail

VAULT_ADDR="%s"
VAULT_SECRET_PATH="%s"
ROLE_ID=$(cat /etc/vault/role-id)
SECRET_ID=$(cat /etc/vault/secret-id)

VAULT_TOKEN=$(curl -sf "${VAULT_ADDR}/v1/auth/approle/login" \
  --data "{\"role_id\":\"${ROLE_ID}\",\"secret_id\":\"${SECRET_ID}\"}" \
  | jq -r '.auth.client_token')
[ -z "${VAULT_TOKEN}" ] && { echo "[ERROR] Vault login failed"; exit 1; }

RKE2_TOKEN=$(curl -sf "${VAULT_ADDR}/v1/${VAULT_SECRET_PATH}" \
  -H "X-Vault-Token: ${VAULT_TOKEN}" \
  | jq -r '.data.data.token')
[ -z "${RKE2_TOKEN}" ] && { echo "[ERROR] Failed to fetch RKE2 token"; exit 1; }

echo "token: ${RKE2_TOKEN}" >> /etc/rancher/rke2/config.yaml
echo "[INFO] RKE2 join token injected into config."

# Remove credentials from disk — no longer needed after first use
rm -f /etc/vault/role-id /etc/vault/secret-id
`, in.VaultParams.Addr, in.VaultParams.SecretPath),
		},
	}
	merged.WriteFiles = append(vaultFiles, merged.WriteFiles...)

	// vault-fetch runs before all other runcmd steps
	merged.RunCmd = append([]interface{}{
		"/var/opt/scripts/vault-fetch-rke2-token.sh",
	}, merged.RunCmd...)

	out, err := yaml.Marshal(merged)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(out), nil
}
