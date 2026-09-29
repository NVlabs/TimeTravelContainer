// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package client

// hostHelpText returns help text as it applies to this host.
func hostHelpText(text string) string { return text }

// explainDaemonExecFailure explains err, the failure to run the ttcd at path.
func explainDaemonExecFailure(path string, err error) error { return err }
