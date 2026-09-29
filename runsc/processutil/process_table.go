// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package processutil

// The process-table functions below report on host processes: PIDs lists
// them, and each other function describes one. A function describing a
// process that has exited returns an error for which
// errors.Is(err, os.ErrNotExist) holds.
//
// StartTime is opaque. It identifies a process across PID reuse, compared
// only with another StartTime from the same host.
//
// State returns the Linux /proc state letter of a process, or the closest
// equivalent: 'R', 'S', 'D', 'T', 'Z', and so on. Callers test for 'Z'.
