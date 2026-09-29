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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hostinet

import "context"

// listenerState remains part of Socket's state schema on Darwin so that the
// platform-independent state code has a stable shape. Hostinet itself is not
// a supported macOS resource; portable checkpoint validation rejects it.
//
// +stateify savable
type listenerState struct {
	addr         []byte
	backlog      int32
	reuseAddr    int32
	reusePort    int32
	v6Only       int32
	bindToDevice string
}

func (s *Socket) beforeSave() {
	s.savedListener = nil
}

func (s *Socket) afterLoad(context.Context) {
	s.fd = -1
}

func restoreListeners() {}
