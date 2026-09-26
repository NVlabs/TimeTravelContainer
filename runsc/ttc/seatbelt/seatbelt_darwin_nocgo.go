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

//go:build darwin && !cgo

package seatbelt

import "errors"

// Apply fails: applying a profile takes cgo, as ttcd's Darwin build has, and
// pure-Go builds, such as tests of the packages that use this one, lack.
func Apply(string) error {
	return errors.New("applying a Seatbelt profile requires a cgo build")
}
