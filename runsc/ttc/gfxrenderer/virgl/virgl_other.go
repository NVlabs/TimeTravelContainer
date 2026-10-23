// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !(darwin || linux) || !cgo

package virgl

import "errors"

// The renderer renders for virgl's guests on macOS and Linux, with cgo.
var errUnavailable = errors.New("the renderer renders for virgl's guests only on macOS and Linux")

// Init fails: see Available.
func Init() error {
	return errUnavailable
}

// Available reports whether the renderer renders for virgl's guests here.
func Available() bool {
	return false
}

// Capsets fails: see Available.
func Capsets() ([]Capset, error) {
	return nil, errUnavailable
}

// Resource is a guest's classic resource.
type Resource struct{}

// NewResource fails: see Available.
func NewResource(id uint32, a ResourceArgs, backing []byte, release func()) (*Resource, error) {
	return nil, errUnavailable
}

// ID returns the resource's ID.
func (r *Resource) ID() uint32 {
	return 0
}

// DecRef releases a reference to r.
func (r *Resource) DecRef() {}

// IncRef takes a reference to r.
func (r *Resource) IncRef() {}

// Backing returns the guest's memory of r.
func (r *Resource) Backing() []byte {
	return nil
}

// Clone fails: see Available.
func (r *Resource) Clone(backing []byte, release func()) (*Resource, error) {
	return nil, errUnavailable
}

// Sync waits for the copies of resources' clones.
func Sync() {}

// Transfer fails: see Available.
func Transfer(r *Resource, a TransferArgs) error {
	return errUnavailable
}

// Context is a virgl context.
type Context struct{}

// NewContext fails: see Available.
func NewContext(id uint32) (*Context, error) {
	return nil, errUnavailable
}

// Attach lets the context use r.
func (c *Context) Attach(r *Resource) {}

// Detach ends the context's use of resource id.
func (c *Context) Detach(id uint32) {}

// Transfer fails: see Available.
func (c *Context) Transfer(r *Resource, a TransferArgs) error {
	return errUnavailable
}

// Submit fails: see Available.
func (c *Context) Submit(buf []byte) error {
	return errUnavailable
}

// Fence is a fence of what a context executed.
type Fence struct{}

// Flush returns a fence of what the context executed.
func (c *Context) Flush() *Fence {
	return &Fence{}
}

// Wait waits for f.
func (f *Fence) Wait() {}

// Destroy destroys the context.
func (c *Context) Destroy() {}

// Clone fails: see Available.
func (c *Context) Clone(resource func(*Resource) (*Resource, error)) (*Context, error) {
	return nil, errUnavailable
}
