// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package images

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type dockerAuthConfig struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

type dockerConfigFile struct {
	Auths       map[string]dockerAuthConfig `json:"auths"`
	CredHelpers map[string]string           `json:"credHelpers"`
	CredsStore  string                      `json:"credsStore"`
}

type dockerHelperResult struct {
	Username string
	Secret   string
}

var credentialHelperName = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]*$")

// dockerCredentialCallback loads Docker's existing per-user configuration.
// Secrets are resolved on demand and are never copied into TTC metadata.
func dockerCredentialCallback() (func(string) (string, string, error), bool, error) {
	name, err := dockerConfigPath()
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading Docker configuration %q: %w", name, err)
	}
	var config dockerConfigFile
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, false, fmt.Errorf("decoding Docker configuration %q: %w", name, err)
	}
	return config.credentials, true, nil
}

func dockerConfigPath() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating Docker configuration: %w", err)
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

func (c dockerConfigFile) credentials(host string) (string, string, error) {
	key, helper, ok := matchingString(c.CredHelpers, host)
	if ok {
		return runDockerCredentialHelper(helper, key)
	}
	if c.CredsStore != "" {
		return runDockerCredentialHelper(c.CredsStore, canonicalRegistryHost(host))
	}
	_, auth, ok := matchingAuth(c.Auths, host)
	if !ok {
		return "", "", nil
	}
	if auth.IdentityToken != "" {
		return "", auth.IdentityToken, nil
	}
	if auth.RegistryToken != "" {
		return "", auth.RegistryToken, nil
	}
	if auth.Username != "" || auth.Password != "" {
		return auth.Username, auth.Password, nil
	}
	if auth.Auth == "" {
		return "", "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(auth.Auth)
	}
	if err != nil {
		return "", "", fmt.Errorf("decoding docker credentials for %q: %w", host, err)
	}
	username, secret, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", fmt.Errorf("docker credentials for %q do not contain a username separator", host)
	}
	return username, secret, nil
}

func matchingString(values map[string]string, host string) (string, string, bool) {
	want := canonicalRegistryHost(host)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if canonicalRegistryHost(key) == want {
			return key, values[key], true
		}
	}
	return "", "", false
}

func matchingAuth(values map[string]dockerAuthConfig, host string) (string, dockerAuthConfig, bool) {
	want := canonicalRegistryHost(host)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if canonicalRegistryHost(key) == want {
			return key, values[key], true
		}
	}
	return "", dockerAuthConfig{}, false
}

func canonicalRegistryHost(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Host
	} else {
		value = strings.SplitN(value, "/", 2)[0]
	}
	value = strings.ToLower(strings.TrimSuffix(value, "/"))
	switch value {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	default:
		return value
	}
}

func runDockerCredentialHelper(helper, server string) (string, string, error) {
	if !credentialHelperName.MatchString(helper) {
		return "", "", fmt.Errorf("invalid docker credential helper %q", helper)
	}
	command := exec.Command("docker-credential-"+helper, "get")
	command.Stdin = strings.NewReader(server + "\n")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		lower := strings.ToLower(message)
		if strings.Contains(lower, "credentials not found") || strings.Contains(lower, "not found in native keychain") {
			return "", "", nil
		}
		if message != "" {
			return "", "", fmt.Errorf("docker credential helper %q: %s: %w", helper, message, err)
		}
		return "", "", fmt.Errorf("docker credential helper %q: %w", helper, err)
	}
	var result dockerHelperResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", "", fmt.Errorf("decoding docker credential helper %q output: %w", helper, err)
	}
	if result.Username == "<token>" {
		return "", result.Secret, nil
	}
	return result.Username, result.Secret, nil
}
