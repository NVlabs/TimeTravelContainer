// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
)

// This build loads what every release in schemas/ saved: each savable type of
// the release has the same fields here, or a migration that accounts for
// every difference (see pkg/state/migrate.go).
func TestThisBuildLoadsEveryReleasesImages(t *testing.T) {
	files, err := filepath.Glob("schemas/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no release schemas in schemas/")
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
