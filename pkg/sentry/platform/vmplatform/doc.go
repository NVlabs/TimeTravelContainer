// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vmplatform provides backend-independent GuestRAM, address-space,
// monitor, and checkpoint execution-cache plumbing for Sentry platforms that
// opt into this VM substrate, currently Darwin HVF and potentially future
// Windows WHPX. Host-specific continuation and vCPU policies are selected
// through VMEngine implementations. platform.Platform remains the universal
// selectable-backend boundary; Linux KVM and systrap are independent of this
// package.
package vmplatform
