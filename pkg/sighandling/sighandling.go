// Copyright 2018 The gVisor Authors.
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

// Package sighandling contains helpers for handling signals to applications.
package sighandling

import (
	"os"
	"os/signal"
	"reflect"

	"gvisor.dev/gvisor/pkg/abi/linux"
)

type forwardedSignal struct {
	host  os.Signal
	guest linux.Signal
}

// handleSignals listens for incoming signals and calls the given handler
// function.
//
// It stops when the stop channel is closed. The done channel is closed once it
// will no longer deliver signals to k.
func handleSignals(sigchans []chan os.Signal, guests []linux.Signal, handler func(linux.Signal), stop, done chan struct{}) {
	// Build a select case.
	sc := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(stop)}}
	for _, sigchan := range sigchans {
		sc = append(sc, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(sigchan)})
	}

	for {
		// Wait for a notification.
		index, _, ok := reflect.Select(sc)

		// Was it the stop channel?
		if index == 0 {
			if !ok {
				// Stop forwarding and notify that it's done.
				close(done)
				return
			}
			continue
		}

		// How about a different close?
		if !ok {
			panic("signal channel closed unexpectedly")
		}

		// Otherwise, translate the host signal represented by this channel to
		// the Linux signal ABI exposed to the container.
		handler(guests[index-1])
	}
}

// StartSignalForwarding ensures that synchronous signals are passed to the
// given handler function and returns a callback that stops signal delivery.
//
// Note that this function permanently takes over signal handling. After the
// stop callback, signals revert to the default Go runtime behavior, which
// cannot be overridden with external calls to signal.Notify.
func StartSignalForwarding(handler func(linux.Signal)) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	// Register individual channels. One channel per standard signal is
	// required as os.Notify() is non-blocking and may drop signals. To avoid
	// this, standard signals have to be queued separately. Channel size 1 is
	// enough for standard signals as their semantics allow de-duplication.
	//
	// External real-time signals are not supported. We rely on the go-runtime
	// for their handling.
	//
	// We do not forward some signals that are likely induced by the behavior
	// of the forwarding process.
	registrations := forwardedSignals()
	sigchans := make([]chan os.Signal, 0, len(registrations))
	guests := make([]linux.Signal, 0, len(registrations))
	for _, registration := range registrations {
		sigchan := make(chan os.Signal, 1)
		sigchans = append(sigchans, sigchan)
		guests = append(guests, registration.guest)
		signal.Notify(sigchan, registration.host)
	}
	// Start up our listener.
	go handleSignals(sigchans, guests, handler, stop, done) // S/R-SAFE: synchronized by Kernel.extMu.

	return func() {
		close(stop)
		<-done
	}
}
