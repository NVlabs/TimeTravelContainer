// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"testing"

	"gvisor.dev/gvisor/pkg/context"
)

// testMemoryManagerWithExhaustibleMemory skips the test: exhausting a memory
// file seals a memfd against growth, which macOS has neither of.
func testMemoryManagerWithExhaustibleMemory(_ context.Context, t *testing.T) (*MemoryManager, func()) {
	t.Skip("exhausting a memory file seals a memfd, which macOS has not")
	return nil, nil
}
