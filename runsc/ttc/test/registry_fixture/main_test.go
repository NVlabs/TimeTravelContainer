// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
)

func TestImageUsesGuestPlatform(t *testing.T) {
	blobs, manifestBytes, err := image([]byte("guest"), "linux", "arm64")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	var manifest manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	configBytes, ok := blobs[manifest.Config.Digest]
	if !ok {
		t.Fatalf("config blob %q is missing", manifest.Config.Digest)
	}
	var config struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if config.OS != "linux" || config.Architecture != "arm64" {
		t.Fatalf("image platform = %s/%s, want linux/arm64", config.OS, config.Architecture)
	}
}
