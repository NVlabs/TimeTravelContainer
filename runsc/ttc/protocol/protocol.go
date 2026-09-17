// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package protocol contains the intentionally tiny TTC client/daemon wire
// contract shared by the thin client and the full daemon implementation.
package protocol

// DaemonAPIVersion is the exact native CLI protocol version. Official TTC
// clients also require an exact build match; SDKs negotiate their own API.
const DaemonAPIVersion = 10

// DaemonQuorumWait is the bounded pre-admission status published by a daemon
// that has opened its persistent Raft transport but cannot yet regain quorum.
type DaemonQuorumWait struct {
	PID        int    `json:"pid"`
	APIVersion int    `json:"api_version"`
	BuildID    string `json:"build_id"`
}

// DaemonQuorumWaitPath returns the advisory local-startup marker used while a
// persistent Raft member is alive but cannot yet regain quorum. The marker is
// deliberately outside the RPC stream: the full daemon request loop remains
// unavailable until the node has recovered execution authority.
func DaemonQuorumWaitPath(socket string) string {
	return socket + ".quorum-wait"
}
