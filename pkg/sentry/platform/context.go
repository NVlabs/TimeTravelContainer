// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"gvisor.dev/gvisor/pkg/context"
)

// contextID is the auth package's type for context.Context.Value keys.
type contextID int

const (
	// CtxPlatform is a Context.Value key for a Platform.
	CtxPlatform contextID = iota

	// CtxFileCoherence is a Context.Value key for the FileCoherence of the
	// Platform that is used to execute the context's application code, if
	// that Platform needs one.
	CtxFileCoherence
)

// FromContext returns the Platform that is used to execute ctx's application
// code, or nil if no such Platform exists.
func FromContext(ctx context.Context) Platform {
	if v := ctx.Value(CtxPlatform); v != nil {
		return v.(Platform)
	}
	return nil
}

// FileCoherenceFromContext returns the FileCoherence of the Platform that is
// used to execute ctx's application code, or nil if that Platform needs none.
func FileCoherenceFromContext(ctx context.Context) FileCoherence {
	if v := ctx.Value(CtxFileCoherence); v != nil {
		return v.(FileCoherence)
	}
	return nil
}
