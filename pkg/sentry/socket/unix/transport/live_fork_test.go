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

package transport

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
)

type liveForkTestIDProvider struct {
	next uint64
}

func (p *liveForkTestIDProvider) UniqueID() uint64 {
	p.next++
	return p.next
}

type liveForkTestRights struct{}

func (*liveForkTestRights) Clone() RightsControlMessage {
	return &liveForkTestRights{}
}

func (*liveForkTestRights) Release(context.Context) {}

func TestValidateLiveForkRejectsQueuedRights(t *testing.T) {
	ctx := context.Background()
	a, b := NewPair(ctx, linux.SOCK_SEQPACKET, &liveForkTestIDProvider{})
	defer a.Close(ctx)
	defer b.Close(ctx)
	if err := ValidateLiveFork(a); err != nil {
		t.Fatalf("empty internal socketpair rejected: %v", err)
	}
	n, notify, err := a.SendMsg(ctx, [][]byte{[]byte("rights")}, ControlMessages{Rights: &liveForkTestRights{}}, nil)
	if err != nil || n != 6 {
		t.Fatalf("SendMsg: got (%d, %v), want (6, nil)", n, err)
	}
	if notify != nil {
		notify()
	}
	if err := ValidateLiveFork(a); err == nil {
		t.Fatal("socketpair with queued SCM_RIGHTS was accepted")
	}
}
