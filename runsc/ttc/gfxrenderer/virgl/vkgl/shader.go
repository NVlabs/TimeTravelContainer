// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"fmt"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tgsi"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl/tospirv"
)

// Shader is a guest's shader: its TGSI, parsed, which the pipelines that
// it is of translate for their state, once of each.
type Shader struct {
	TGSI *tgsi.Shader
	// id is the shader's, of pipelines' keys.
	id uint64

	mu           sync.Mutex
	translations map[string]*tospirv.Result
}

var shaderIDs atomic.Uint64

// NewShader returns a shader of text, a guest's TGSI.
func NewShader(text string) (*Shader, error) {
	s, err := tgsi.Parse(text)
	if err != nil {
		return nil, err
	}
	return &Shader{TGSI: s, id: shaderIDs.Add(1), translations: make(map[string]*tospirv.Result)}, nil
}

// optionsKey returns a key of opts, of which translations are the same.
func optionsKey(opts *tospirv.Options) string {
	return fmt.Sprintf("%v", *opts)
}

// Translate returns the shader's translation for opts, translating it the
// first time. The result's words are the caller's copy, which linking may
// change.
func (s *Shader) Translate(opts tospirv.Options) (*tospirv.Result, error) {
	key := optionsKey(&opts)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.translations[key]
	if !ok {
		var err error
		if res, err = tospirv.Translate(s.TGSI, opts); err != nil {
			return nil, err
		}
		s.translations[key] = res
	}
	cp := *res
	cp.Words = append([]uint32(nil), res.Words...)
	return &cp, nil
}
