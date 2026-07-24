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

// Binary registry_fixture serves one deterministic OCI image from an in-process
// Registry V2 endpoint. It is intentionally small and exists only for TTC
// integration testing without a public registry dependency.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	configMediaType   = "application/vnd.oci.image.config.v1+json"
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	layerMediaType    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func image(binary []byte) (map[string][]byte, []byte, error) {
	var tarData bytes.Buffer
	tw := tar.NewWriter(&tarData)
	header := &tar.Header{
		Name:    "bin/fixture",
		Mode:    0755,
		Size:    int64(len(binary)),
		ModTime: time.Unix(0, 0),
	}
	if err := tw.WriteHeader(header); err != nil {
		return nil, nil, err
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}

	var layer bytes.Buffer
	zw, err := gzip.NewWriterLevel(&layer, gzip.BestSpeed)
	if err != nil {
		return nil, nil, err
	}
	zw.Header.ModTime = time.Unix(0, 0)
	zw.Header.OS = 255
	if _, err := zw.Write(tarData.Bytes()); err != nil {
		return nil, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}

	config, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           runtime.GOOS,
		"config": map[string]any{
			"Entrypoint": []string{"/bin/fixture"},
			"Env":        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			"WorkingDir": "/",
		},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{digest(tarData.Bytes())},
		},
	})
	if err != nil {
		return nil, nil, err
	}
	layerBytes := layer.Bytes()
	manifestBytes, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config:        descriptor{MediaType: configMediaType, Digest: digest(config), Size: len(config)},
		Layers:        []descriptor{{MediaType: layerMediaType, Digest: digest(layerBytes), Size: len(layerBytes)}},
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string][]byte{digest(config): config, digest(layerBytes): layerBytes}, manifestBytes, nil
}

func serveContent(w http.ResponseWriter, r *http.Request, mediaType, contentDigest string, content []byte) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	if contentDigest != "" {
		w.Header().Set("Docker-Content-Digest", contentDigest)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, bytes.NewReader(content))
	}
}

func main() {
	binaryPath := flag.String("binary", "", "static guest binary to place at /bin/fixture")
	addressFile := flag.String("address-file", "", "file in which to publish host:port")
	flag.Parse()
	if *binaryPath == "" || *addressFile == "" {
		log.Fatal("--binary and --address-file are required")
	}
	binary, err := os.ReadFile(*binaryPath)
	if err != nil {
		log.Fatalf("read guest binary: %v", err)
	}
	blobs, manifestBytes, err := image(binary)
	if err != nil {
		log.Fatalf("build image: %v", err)
	}
	manifestDigest := digest(manifestBytes)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
			return
		}
		serveContent(w, r, "application/json", "", []byte("{}"))
	})
	mux.HandleFunc("/v2/ttc/fixture/manifests/", func(w http.ResponseWriter, r *http.Request) {
		selector := strings.TrimPrefix(r.URL.Path, "/v2/ttc/fixture/manifests/")
		if selector != "test" && selector != manifestDigest {
			http.NotFound(w, r)
			return
		}
		serveContent(w, r, manifestMediaType, manifestDigest, manifestBytes)
	})
	mux.HandleFunc("/v2/ttc/fixture/blobs/", func(w http.ResponseWriter, r *http.Request) {
		blobDigest := strings.TrimPrefix(r.URL.Path, "/v2/ttc/fixture/blobs/")
		blob, ok := blobs[blobDigest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		serveContent(w, r, "application/octet-stream", blobDigest, blob)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		log.Fatalf("split listener address: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*addressFile), 0700); err != nil {
		log.Fatalf("create address directory: %v", err)
	}
	if err := os.WriteFile(*addressFile, []byte(fmt.Sprintf("localhost:%s/ttc/fixture:test\n", port)), 0600); err != nil {
		log.Fatalf("write address: %v", err)
	}
	if err := http.Serve(listener, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
