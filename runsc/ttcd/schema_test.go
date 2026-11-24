// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
)

// This build loads what every release of its platform in schemas/ saved: each
// savable type of the release has the same fields here, or a migration that
// accounts for every difference (see pkg/state/migrate.go). A platform saves
// types of its own, and images restore only on their own platform.
func TestThisBuildLoadsEveryReleasesImages(t *testing.T) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	files, err := filepath.Glob(filepath.Join("schemas", platform, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skipf("no release schemas of %s in schemas/%s", platform, platform)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]state.SchemaType
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var names []string
		for name := range schema {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := state.CheckMigration(name, schema[name]); err != nil {
				t.Errorf("%s: %v", filepath.Base(file), err)
			}
		}
	}
}
