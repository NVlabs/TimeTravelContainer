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
