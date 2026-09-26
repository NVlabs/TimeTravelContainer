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

package checkpoint

import (
	"strings"
	"testing"
)

func TestPortableContractRoundTrip(t *testing.T) {
	want := NewPortableContract("release-1", "arm64", []string{"crc32", "aes", "aes"}, ResourceProfileTTCManaged, []string{"memory-4k", "state-v1"})
	metadata := make(map[string]string)
	if err := want.AddToMetadata(metadata); err != nil {
		t.Fatalf("AddToMetadata: %v", err)
	}
	got, err := PortableContractFromMetadata(metadata)
	if err != nil {
		t.Fatalf("PortableContractFromMetadata: %v", err)
	}
	if err := got.ValidateDestination("release-1", "arm64", []string{"aes", "crc32", "sha2"}, []string{"state-v1", "memory-4k"}); err != nil {
		t.Fatalf("ValidateDestination: %v", err)
	}
	if !got.IsCrossHostPortable() {
		t.Error("managed contract is not cross-host portable")
	}
}

func TestPortableContractRejectsLegacyAndMismatch(t *testing.T) {
	if _, err := PortableContractFromMetadata(map[string]string{"runsc_version": "legacy"}); err == nil {
		t.Fatal("legacy metadata unexpectedly accepted")
	}
	c := NewPortableContract("release-1", "arm64", []string{"aes", "crc32"}, ResourceProfileHostBound, []string{"state-v1"})
	if c.IsCrossHostPortable() {
		t.Error("host-bound contract reported as portable")
	}
	err := c.ValidateDestination("release-1", "arm64", []string{"aes"}, []string{"state-v1"})
	if err == nil || !strings.Contains(err.Error(), "crc32") {
		t.Fatalf("CPU mismatch error = %v, want explicit crc32", err)
	}
	if err := c.ValidateDestination("release-2", "arm64", c.CPUFeatures, c.RequiredCapabilities); err == nil {
		t.Error("state ABI mismatch unexpectedly accepted")
	}
}
