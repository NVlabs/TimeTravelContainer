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

package images

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// registryStallTimeout is how long a registry response may go without
// delivering its headers or any data before the pull gives up on its
// connection. A registry or CDN can stall a connection without closing it,
// and a pull would otherwise wait for it forever.
const registryStallTimeout = 30 * time.Second

// newRegistryClient returns an HTTP client for registry requests that gives up
// on a response that stalls for timeout.
func newRegistryClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: stallTimeoutTransport{RoundTripper: transport, timeout: timeout}}
}

// stallTimeoutTransport limits how long a read of a response body may make no
// progress. Such a read fails with io.ErrUnexpectedEOF, on which containerd's
// fetcher reconnects and resumes the blob at the same offset.
type stallTimeoutTransport struct {
	http.RoundTripper
	timeout time.Duration
}

// RoundTrip implements http.RoundTripper.RoundTrip.
func (t stallTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = newStallTimeoutBody(resp.Body, t.timeout)
	return resp, nil
}

type stallTimeoutBody struct {
	body    io.ReadCloser
	timeout time.Duration
	// timer closes body if it fires, which ends a pending read of it.
	timer   *time.Timer
	stalled atomic.Bool
}

func newStallTimeoutBody(body io.ReadCloser, timeout time.Duration) *stallTimeoutBody {
	b := &stallTimeoutBody{body: body, timeout: timeout}
	b.timer = time.AfterFunc(timeout, func() {
		b.stalled.Store(true)
		_ = body.Close()
	})
	b.timer.Stop()
	return b
}

// Read implements io.Reader.Read.
func (b *stallTimeoutBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.timeout)
	n, err := b.body.Read(p)
	b.timer.Stop()
	if err != nil && b.stalled.Load() {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// Close implements io.Closer.Close.
func (b *stallTimeoutBody) Close() error {
	b.timer.Stop()
	return b.body.Close()
}
