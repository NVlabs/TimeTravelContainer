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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestPublishRemoteTargetRetriesPausedAuthorityGap(t *testing.T) {
	const transactionID = "fork-publish-retry"
	var publishes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tx := cluster.Transaction{State: cluster.TransactionTargetPaused, Revision: 7}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/forks/"+transactionID:
			_ = json.NewEncoder(w).Encode(tx)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/forks/"+transactionID+"/publish":
			if publishes.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "remote fork target node has no valid execution authority"})
				return
			}
			tx.State = cluster.TransactionPublished
			tx.Revision++
			_ = json.NewEncoder(w).Encode(tx)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	peer, err := cluster.NewPlainClient(server.URL, "source", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := publishRemoteTarget(ctx, peer, transactionID, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != cluster.TransactionPublished || publishes.Load() != 2 {
		t.Fatalf("publish result=(%s, attempts=%d), want (published, 2)", tx.State, publishes.Load())
	}
}
