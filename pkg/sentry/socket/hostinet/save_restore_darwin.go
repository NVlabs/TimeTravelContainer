//go:build darwin

package hostinet

import "context"

// listenerState remains part of Socket's state schema on Darwin so that the
// platform-independent state code has a stable shape. Hostinet itself is not
// a supported macOS resource; portable checkpoint validation rejects it.
//
// +stateify savable
type listenerState struct {
	addr         []byte
	backlog      int32
	reuseAddr    int32
	reusePort    int32
	v6Only       int32
	bindToDevice string
}

func (s *Socket) beforeSave() {
	s.savedListener = nil
}

func (s *Socket) afterLoad(context.Context) {
	s.fd = -1
}

func restoreListeners() {}
