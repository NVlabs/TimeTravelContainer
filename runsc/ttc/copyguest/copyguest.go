// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package copyguest provides the Linux executable that container copy
// operations run in the guest: a small one, which starts there in a fraction
// of the time a TTC executable takes, and runs there on hosts whose TTC
// executable can't.
package copyguest
