// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"net"
	"os"
)

// dialGFXBroker connects to the GPU renderers' broker at path, and returns
// the connection for the Sentry.
func dialGFXBroker(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("--gfx needs --gfx-broker, the socket of the GPU renderers' broker")
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connecting to the GPU renderers' broker: %w", err)
	}
	defer conn.Close()
	file, err := conn.File()
	if err != nil {
		return nil, fmt.Errorf("connecting to the GPU renderers' broker: %w", err)
	}
	return file, nil
}
