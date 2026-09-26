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

//go:build darwin

// Package coretag implements Linux core scheduling tags.
package coretag

import "errors"

var errUnavailable = errors.New("Linux core scheduling tags are unavailable on Darwin")

// Enable reports that core scheduling cannot be enabled on Darwin.
func Enable() error {
	return errUnavailable
}

// GetAllCoreTags reports that core scheduling cannot be queried on Darwin.
func GetAllCoreTags(_ int) ([]uint64, error) {
	return nil, errUnavailable
}
