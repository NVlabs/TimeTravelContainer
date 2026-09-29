// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcremote

import (
	"encoding/json"
	"fmt"
	"io"
)

const traceVersion = 1

// Trace is a complete, replayable model execution. InitialValues represent
// fork-time page contents and Events retain their original execution order.
type Trace struct {
	Version       int      `json:"version"`
	Epoch         uint64   `json:"epoch"`
	InitialValues []uint64 `json:"initial_values"`
	Events        []Event  `json:"events"`
}

// NewTrace constructs an empty trace.
func NewTrace(epoch uint64, initialValues []uint64) *Trace {
	return &Trace{
		Version:       traceVersion,
		Epoch:         epoch,
		InitialValues: append([]uint64(nil), initialValues...),
	}
}

// Append applies e to model and records it only if it succeeds.
func (t *Trace) Append(model *Model, e Event) error {
	if model.Epoch() != t.Epoch {
		return fmt.Errorf("model epoch %d does not match trace epoch %d", model.Epoch(), t.Epoch)
	}
	if err := model.Apply(e); err != nil {
		return err
	}
	t.Events = append(t.Events, e)
	return nil
}

// Replay applies all trace events and returns the final model.
func (t *Trace) Replay() (*Model, error) {
	if t.Version != traceVersion {
		return nil, fmt.Errorf("unsupported trace version %d", t.Version)
	}
	m := NewModel(t.Epoch, t.InitialValues)
	for i, event := range t.Events {
		if err := m.Apply(event); err != nil {
			return nil, fmt.Errorf("event %d (%s page=%d transfer=%d): %w", i, event.Kind, event.Page, event.Transfer, err)
		}
	}
	return m, nil
}

// EncodeTrace writes t as JSON.
func EncodeTrace(w io.Writer, t *Trace) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(t)
}

// DecodeTrace reads and validates one JSON trace. Trailing JSON values are
// rejected to make stored failure artifacts unambiguous.
func DecodeTrace(r io.Reader) (*Trace, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var trace Trace
	if err := dec.Decode(&trace); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trace contains a trailing JSON value")
		}
		return nil, fmt.Errorf("reading trailing trace data: %w", err)
	}
	if trace.Version != traceVersion {
		return nil, fmt.Errorf("unsupported trace version %d", trace.Version)
	}
	return &trace, nil
}
