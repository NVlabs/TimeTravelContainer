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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const testStallTimeout = 200 * time.Millisecond

// stallAfter writes the first n bytes of data, then stalls until the client
// gives up on the request.
func stallAfter(w http.ResponseWriter, r *http.Request, data []byte, n int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data[:n])
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func TestStalledRegistryResponseFailsAsUnexpectedEOF(t *testing.T) {
	data := []byte("first second")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stallAfter(w, r, data, len("first"))
	}))
	defer server.Close()

	resp, err := newRegistryClient(testStallTimeout).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make([]byte, len("first"))
	if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != "first" {
		t.Fatalf("reading the data before the stall: got %q, err %v", got, err)
	}
	started := time.Now()
	if _, err := resp.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading the stalled response: got %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if elapsed := time.Since(started); elapsed > 20*testStallTimeout {
		t.Errorf("the stalled read took %v to fail, want about %v", elapsed, testStallTimeout)
	}
}

func TestRegistryResponseSlowerThanTheStallTimeoutCompletes(t *testing.T) {
	data := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		// Each chunk arrives within the stall timeout, though the whole
		// response takes longer than it.
		for i := range data {
			_, _ = w.Write(data[i : i+1])
			w.(http.Flusher).Flush()
			time.Sleep(testStallTimeout / 4)
		}
	}))
	defer server.Close()

	resp, err := newRegistryClient(testStallTimeout).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("got %q, err %v; want %q", got, err, data)
	}
}

func TestPullResumesAStalledBlob(t *testing.T) {
	blob := bytes.Repeat([]byte("layer data "), 4096)
	sum := sha256.Sum256(blob)
	dgst := "sha256:" + hex.EncodeToString(sum[:])
	var desc ocispec.Descriptor
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":%d}`, ocispec.MediaTypeImageLayerGzip, dgst, len(blob))), &desc); err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/test/image/blobs/"+dgst {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && gets.Add(1) == 1 {
			// The first download stalls halfway, as a CDN may.
			stallAfter(w, r, blob, len(blob)/2)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	}))
	defer server.Close()

	client := newRegistryClient(testStallTimeout)
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: docker.ConfigureDefaultRegistries(
			docker.WithAuthorizer(docker.NewDockerAuthorizer(docker.WithAuthClient(client))),
			docker.WithClient(client),
			docker.WithPlainHTTP(docker.MatchLocalhost),
		),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fetcher, err := resolver.Fetcher(ctx, strings.TrimPrefix(server.URL, "http://")+"/test/image")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the blob: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("read %d bytes that differ from the %d-byte blob", len(got), len(blob))
	}
	if n := gets.Load(); n < 2 {
		t.Errorf("blob downloaded with %d requests, want the stalled one and a resumption", n)
	}
}
