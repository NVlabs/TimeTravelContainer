// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	gerrors "gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/waiter"
)

// testCapset stands in for gfxstream's Vulkan capability set.
var testCapset = []byte("gfxstream vulkan capability set, version 1")

// fakeRenderer is a Renderer for tests. It records what the device asks of
// it, backs resources with the kernel's memory, and signals fences when a
// test tells it to.
type fakeRenderer struct {
	mf     *pgalloc.MemoryFile
	events RendererEvents

	mu        sync.Mutex
	calls     []string
	sizes     map[uint32]uint64
	memory    map[uint32]*fakeMemory
	fences    []Fence
	closed    bool
	rejectCtx bool
	// restored is the memory of resources that a restore gave the renderer,
	// which their mappings start with.
	restored map[uint32][]byte
	// failRestore makes restores fail.
	failRestore bool
	// referenceSnapshots makes snapshots references, as the Venus
	// renderer's are.
	referenceSnapshots bool
}

// fakeRenderers starts fakeRenderers.
type fakeRenderers struct {
	mf *pgalloc.MemoryFile

	mu        sync.Mutex
	renderers []*fakeRenderer
	// rejectCapset0 makes new renderers reject contexts of capability set 0.
	rejectCapset0 bool
	// failRestore makes new renderers fail to restore snapshots.
	failRestore bool
	// referenceSnapshots makes new renderers' snapshots references.
	referenceSnapshots bool
}

func (f *fakeRenderers) newRenderer(events RendererEvents) (Renderer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &fakeRenderer{
		mf:          f.mf,
		events:      events,
		sizes:       make(map[uint32]uint64),
		memory:      make(map[uint32]*fakeMemory),
		rejectCtx:   f.rejectCapset0,
		failRestore: f.failRestore,

		referenceSnapshots: f.referenceSnapshots,
	}
	f.renderers = append(f.renderers, r)
	return r, nil
}

// last returns the renderer started last.
func (f *fakeRenderers) last(t *testing.T) *fakeRenderer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.renderers) == 0 {
		t.Fatal("no renderer was started")
	}
	return f.renderers[len(f.renderers)-1]
}

func (f *fakeRenderers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.renderers)
}

func (r *fakeRenderer) record(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

// takeCalls returns the calls recorded since the last takeCalls.
func (r *fakeRenderer) takeCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

// CreateContext implements Renderer.CreateContext.
func (r *fakeRenderer) CreateContext(ctxID, capsetID uint32, name string) error {
	r.record("CreateContext %d capset %d %q", ctxID, capsetID, name)
	if capsetID == 0 && r.rejectCtx {
		return errors.New("no default capability set")
	}
	return nil
}

// DestroyContext implements Renderer.DestroyContext.
func (r *fakeRenderer) DestroyContext(ctxID uint32) {
	r.record("DestroyContext %d", ctxID)
}

// CreateResource implements Renderer.CreateResource.
func (r *fakeRenderer) CreateResource(ctxID, resID uint32, args ResourceArgs) error {
	r.record("CreateResource %d %d %dx%d format %d size %d", ctxID, resID, args.Width, args.Height, args.Format, args.Size)
	r.mu.Lock()
	r.sizes[resID] = args.Size
	r.mu.Unlock()
	return nil
}

// CreateBlob implements Renderer.CreateBlob.
func (r *fakeRenderer) CreateBlob(ctxID, resID uint32, args BlobArgs) error {
	r.record("CreateBlob %d %d mem %d flags %d id %d size %d", ctxID, resID, args.Mem, args.Flags, args.ID, args.Size)
	r.mu.Lock()
	r.sizes[resID] = args.Size
	r.mu.Unlock()
	return nil
}

// MapResource implements Renderer.MapResource.
func (r *fakeRenderer) MapResource(resID uint32) (ResourceMemory, error) {
	r.record("MapResource %d", resID)
	r.mu.Lock()
	defer r.mu.Unlock()
	size, ok := r.sizes[resID]
	if !ok {
		return nil, fmt.Errorf("no resource %d", resID)
	}
	length, ok := hostarch.PageRoundUp(size)
	if !ok {
		return nil, fmt.Errorf("resource %d is too large", resID)
	}
	fr, err := r.mf.Allocate(length, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		return nil, err
	}
	mem := &fakeMemory{mf: r.mf, fr: fr}
	if data, ok := r.restored[resID]; ok {
		if err := mem.copyIn(data); err != nil {
			return nil, err
		}
	}
	r.memory[resID] = mem
	return mem, nil
}

// fakeSnapshot is a fakeRenderer's snapshot: its resources' sizes, and what
// their memory holds.
type fakeSnapshot struct {
	Sizes  map[uint32]uint64
	Memory map[uint32][]byte
	// Reference tells reference snapshots apart, as the tokens that refer
	// to what the renderer keeps are.
	Reference uint64 `json:",omitempty"`
}

// fakeReferences numbers fake reference snapshots.
var fakeReferences atomic.Uint64

// Snapshot implements Renderer.Snapshot.
func (r *fakeRenderer) Snapshot() (Snapshot, error) {
	r.record("Snapshot")
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := fakeSnapshot{Sizes: maps.Clone(r.sizes), Memory: make(map[uint32][]byte)}
	for id, mem := range r.memory {
		if mem.released {
			continue
		}
		data, err := mem.copyOut()
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Memory[id] = data
	}
	if r.referenceSnapshots {
		snapshot.Reference = fakeReferences.Add(1)
	}
	data, err := json.Marshal(&snapshot)
	return Snapshot{Data: data, Reference: r.referenceSnapshots}, err
}

// ForgetSnapshot implements Renderer.ForgetSnapshot.
func (r *fakeRenderer) ForgetSnapshot(snapshot []byte) {
	r.record("ForgetSnapshot")
}

// Restore implements Renderer.Restore.
func (r *fakeRenderer) Restore(data []byte, sizes map[uint32]uint64, last bool) error {
	if last {
		r.record("Restore %v", slices.Sorted(maps.Keys(sizes)))
	} else {
		r.record("Restore %v, not the last", slices.Sorted(maps.Keys(sizes)))
	}
	var snapshot fakeSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRestore {
		return errors.New("the restore failed")
	}
	for id, size := range sizes {
		if snapshot.Sizes[id] != size {
			return fmt.Errorf("resource %d is %d bytes, but %d in the snapshot", id, size, snapshot.Sizes[id])
		}
	}
	r.sizes = snapshot.Sizes
	r.restored = snapshot.Memory
	return nil
}

// AttachResource implements Renderer.AttachResource.
func (r *fakeRenderer) AttachResource(ctxID, resID uint32) {
	r.record("AttachResource %d %d", ctxID, resID)
}

// DetachResource implements Renderer.DetachResource.
func (r *fakeRenderer) DetachResource(ctxID, resID uint32) {
	r.record("DetachResource %d %d", ctxID, resID)
}

// DestroyResource implements Renderer.DestroyResource.
func (r *fakeRenderer) DestroyResource(resID uint32) {
	r.record("DestroyResource %d", resID)
}

// SubmitCommand implements Renderer.SubmitCommand.
func (r *fakeRenderer) SubmitCommand(ctxID uint32, cmd []byte) error {
	r.record("SubmitCommand %d %q", ctxID, cmd)
	return nil
}

// Transfer implements Renderer.Transfer.
func (r *fakeRenderer) Transfer(ctxID, resID uint32, args TransferArgs) error {
	r.record("Transfer %d %d to host %t box %d,%d %dx%d", ctxID, resID, args.ToHost, args.X, args.Y, args.W, args.H)
	return nil
}

// CreateFence implements Renderer.CreateFence.
func (r *fakeRenderer) CreateFence(f Fence) error {
	r.record("CreateFence %d ctx %d ring %t %d", f.ID, f.CtxID, f.HasRing, f.Ring)
	r.mu.Lock()
	r.fences = append(r.fences, f)
	r.mu.Unlock()
	return nil
}

// Close implements Renderer.Close.
func (r *fakeRenderer) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *fakeRenderer) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

// fence returns the fence with the given ID.
func (r *fakeRenderer) fence(t *testing.T, id uint64) Fence {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.fences {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("the renderer has no fence %d", id)
	return Fence{}
}

// signal signals the fence with the given ID.
func (r *fakeRenderer) signal(t *testing.T, id uint64) {
	t.Helper()
	r.events.FenceSignaled(r.fence(t, id))
}

// fakeMemory is the memory of a fakeRenderer's resource.
type fakeMemory struct {
	mf       *pgalloc.MemoryFile
	fr       memmap.FileRange
	released bool
}

func (m *fakeMemory) File() memmap.File { return m.mf }
func (m *fakeMemory) Offset() uint64    { return m.fr.Start }
func (m *fakeMemory) Release() {
	m.released = true
	m.mf.DecRef(m.fr)
}

// copyOut returns what the memory holds.
func (m *fakeMemory) copyOut() ([]byte, error) {
	buf := make([]byte, m.fr.Length())
	ims, err := m.mf.MapInternal(m.fr, hostarch.Read)
	if err != nil {
		return nil, err
	}
	_, err = safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), ims)
	return buf, err
}

// copyIn fills the memory from data.
func (m *fakeMemory) copyIn(data []byte) error {
	ims, err := m.mf.MapInternal(m.fr, hostarch.Write)
	if err != nil {
		return err
	}
	_, err = safemem.CopySeq(ims, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data[:min(uint64(len(data)), m.fr.Length())])))
	return err
}

func (m *fakeMemory) read(t *testing.T, off, n uint64) []byte {
	t.Helper()
	buf := make([]byte, n)
	ims, err := m.mf.MapInternal(memmap.FileRange{Start: m.fr.Start + off, End: m.fr.Start + off + n}, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), ims); err != nil {
		t.Fatalf("CopySeq: %v", err)
	}
	return buf
}

func (m *fakeMemory) write(t *testing.T, off uint64, data []byte) {
	t.Helper()
	ims, err := m.mf.MapInternal(memmap.FileRange{Start: m.fr.Start + off, End: m.fr.Start + off + uint64(len(data))}, hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.CopySeq(ims, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))); err != nil {
		t.Fatalf("CopySeq: %v", err)
	}
}

// testBlocker is a task that blocks on the test's goroutine, which is not
// the task's goroutine.
type testBlocker struct {
	*kernel.Task
}

// BlockWithTimeout implements context.Context.BlockWithTimeout.
func (b testBlocker) BlockWithTimeout(ch chan struct{}, haveTimeout bool, timeout time.Duration) (time.Duration, error) {
	var timer <-chan time.Time
	if haveTimeout {
		timer = time.After(timeout)
	}
	select {
	case <-ch:
		return timeout, nil
	case <-timer:
		return 0, linuxerr.ETIMEDOUT
	}
}

const scratchSize = 1 << 20

// testEnv is a kernel with a virtual GPU, and a task that uses it.
type testEnv struct {
	t       *testing.T
	k       *kernel.Kernel
	task    *kernel.Task
	ctx     context.Context
	creds   *auth.Credentials
	root    vfs.VirtualDentry
	fakes   *fakeRenderers
	scratch hostarch.Addr
	// next is the offset of the next unused scratch memory.
	next uint64
}

func newTestEnv(t *testing.T, policy ForkPolicy) *testEnv {
	t.Helper()
	k, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	ctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	fakes := &fakeRenderers{mf: k.MemoryFile()}
	if err := Register(k.VFS(), Options{
		Capsets:     []Capset{{ID: linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, MaxVersion: 1, Data: testCapset}},
		NewRenderer: fakes.newRenderer,
		ForkPolicy:  policy,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mns, err := k.VFS().NewMountNamespace(ctx, creds, "", tmpfs.Name, &vfs.MountOptions{}, nil)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	root := mns.Root(ctx)
	for _, dir := range []string{"/dev", "/dev/dri"} {
		if err := k.VFS().MkdirAt(ctx, creds, pathOp(root, dir), &vfs.MkdirOptions{Mode: 0755}); err != nil {
			t.Fatalf("MkdirAt(%s): %v", dir, err)
		}
	}
	if err := k.VFS().MknodAt(ctx, creds, pathOp(root, "/dev/"+renderNodePath), &vfs.MknodOptions{
		Mode:     linux.S_IFCHR | 0666,
		DevMajor: linux.DRM_MAJOR,
		DevMinor: linux.DRM_RENDER_MINOR_BASE,
	}); err != nil {
		t.Fatalf("MknodAt: %v", err)
	}
	ls, err := limits.NewLinuxLimitSet()
	if err != nil {
		t.Fatalf("NewLinuxLimitSet: %v", err)
	}
	tg := k.NewThreadGroup(k.RootPIDNamespace(), kernel.NewSignalHandlers(), linux.SIGCHLD, ls)
	task, err := testutil.CreateTask(ctx, "gpu-test", tg, mns, root, root)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mm := task.MemoryManager()
	if _, err := mm.SetMmapLayout(arch.New(arch.Host), ls, false /* randomize */); err != nil {
		t.Fatalf("SetMmapLayout: %v", err)
	}
	scratch, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   scratchSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	return &testEnv{
		t:       t,
		k:       k,
		task:    task,
		ctx:     testBlocker{task},
		creds:   creds,
		root:    root,
		fakes:   fakes,
		scratch: scratch,
		next:    hostarch.PageSize,
	}
}

func pathOp(root vfs.VirtualDentry, path string) *vfs.PathOperation {
	return &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(path)}
}

// open opens the render node.
func (e *testEnv) open() *vfs.FileDescription {
	e.t.Helper()
	fd, err := e.k.VFS().OpenAt(e.task, e.creds, pathOp(e.root, "/dev/"+renderNodePath), &vfs.OpenOptions{Flags: linux.O_RDWR})
	if err != nil {
		e.t.Fatalf("opening the render node: %v", err)
	}
	e.t.Cleanup(func() { fd.DecRef(e.task) })
	return fd
}

// put copies data to new scratch memory, and returns its address.
func (e *testEnv) put(data []byte) hostarch.Addr {
	e.t.Helper()
	addr := e.alloc(uint64(len(data)))
	if _, err := e.task.CopyOutBytes(addr, data); err != nil {
		e.t.Fatalf("CopyOutBytes: %v", err)
	}
	return addr
}

// alloc returns the address of n bytes of new scratch memory.
func (e *testEnv) alloc(n uint64) hostarch.Addr {
	e.t.Helper()
	addr := e.scratch + hostarch.Addr(e.next)
	e.next += (n + 7) &^ 7
	if e.next > scratchSize {
		e.t.Fatal("out of scratch memory")
	}
	return addr
}

// get returns n bytes of the task's memory at addr.
func (e *testEnv) get(addr hostarch.Addr, n int) []byte {
	e.t.Helper()
	buf := make([]byte, n)
	if _, err := e.task.CopyInBytes(addr, buf); err != nil {
		e.t.Fatalf("CopyInBytes: %v", err)
	}
	return buf
}

// ioctl makes the ioctl cmd on file with arg, which it copies to the task's
// memory and back.
func (e *testEnv) ioctl(file *vfs.FileDescription, cmd uint32, arg marshal.Marshallable) error {
	e.t.Helper()
	if _, err := arg.CopyOut(e.task, e.scratch); err != nil {
		e.t.Fatalf("CopyOut: %v", err)
	}
	var args arch.SyscallArguments
	args[1].Value = uintptr(cmd)
	args[2].Value = uintptr(e.scratch)
	_, err := file.Ioctl(e.ctx, e.task.MemoryManager(), 0, args)
	if _, err := arg.CopyIn(e.task, e.scratch); err != nil {
		e.t.Fatalf("CopyIn: %v", err)
	}
	return err
}

// file returns the file of fd, with a reference that the test drops.
func (e *testEnv) file(fd int32) *vfs.FileDescription {
	e.t.Helper()
	file := e.task.GetFile(fd)
	if file == nil {
		e.t.Fatalf("no file %d", fd)
	}
	e.t.Cleanup(func() { file.DecRef(e.task) })
	return file
}

// closeFD closes fd.
func (e *testEnv) closeFD(fd int32) {
	e.t.Helper()
	file := e.task.FDTable().Remove(e.task, fd)
	if file == nil {
		e.t.Fatalf("no file %d", fd)
	}
	file.DecRef(e.task)
}

func drmIOWR(nr uint32, size int) uint32 {
	return linux.IOWR(linux.DRM_IOCTL_BASE, nr, uint32(size))
}

func virtgpuIOWR(nr uint32, size int) uint32 {
	return drmIOWR(linux.DRM_COMMAND_BASE+nr, size)
}

// contextInit creates file's context with the given capability set and
// number of rings.
func (e *testEnv) contextInit(file *vfs.FileDescription, capset, rings uint64, name string) error {
	e.t.Helper()
	params := []linux.DRMVirtgpuContextSetParam{
		{Param: linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID, Value: capset},
		{Param: linux.VIRTGPU_CONTEXT_PARAM_NUM_RINGS, Value: rings},
	}
	if name != "" {
		params = append(params, linux.DRMVirtgpuContextSetParam{
			Param: linux.VIRTGPU_CONTEXT_PARAM_DEBUG_NAME,
			Value: uint64(e.put(append([]byte(name), 0))),
		})
	}
	return e.contextInitParams(file, params)
}

func (e *testEnv) contextInitParams(file *vfs.FileDescription, params []linux.DRMVirtgpuContextSetParam) error {
	e.t.Helper()
	addr := e.alloc(uint64(len(params)) * 16)
	if len(params) != 0 {
		if _, err := linux.CopyDRMVirtgpuContextSetParamSliceOut(e.task, addr, params); err != nil {
			e.t.Fatalf("copying out context parameters: %v", err)
		}
	}
	ci := linux.DRMVirtgpuContextInit{NumParams: uint32(len(params)), CtxSetParams: uint64(addr)}
	return e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_CONTEXT_INIT, ci.SizeBytes()), &ci)
}

// createBlob creates a blob, and returns its handle and resource ID.
func (e *testEnv) createBlob(file *vfs.FileDescription, mem, flags uint32, id, size uint64, cmd []byte) (uint32, uint32, error) {
	e.t.Helper()
	rc := linux.DRMVirtgpuResourceCreateBlob{
		BlobMem:   mem,
		BlobFlags: flags,
		Size:      size,
		BlobID:    id,
	}
	if cmd != nil {
		rc.Cmd = uint64(e.put(cmd))
		rc.CmdSize = uint32(len(cmd))
	}
	err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_RESOURCE_CREATE_BLOB, rc.SizeBytes()), &rc)
	return rc.BOHandle, rc.ResHandle, err
}

// mustCreateBlob creates a mappable blob of size bytes.
func (e *testEnv) mustCreateBlob(file *vfs.FileDescription, flags uint32, size uint64) (uint32, uint32) {
	e.t.Helper()
	handle, resID, err := e.createBlob(file, linux.VIRTGPU_BLOB_MEM_HOST3D, flags, 1, size, nil)
	if err != nil {
		e.t.Fatalf("creating a blob: %v", err)
	}
	return handle, resID
}

// execbuffer submits cmd, and returns the fd of its fence, if it asks for
// one.
func (e *testEnv) execbuffer(file *vfs.FileDescription, flags, ring uint32, inFence int32, cmd []byte, handles ...uint32) (int32, error) {
	e.t.Helper()
	eb := linux.DRMVirtgpuExecbuffer{
		Flags:   flags,
		Size:    uint32(len(cmd)),
		Command: uint64(e.put(cmd)),
		RingIdx: ring,
		FenceFD: inFence,
	}
	if len(handles) != 0 {
		addr := e.alloc(uint64(4 * len(handles)))
		if _, err := primitive.CopyUint32SliceOut(e.task, addr, handles); err != nil {
			e.t.Fatalf("copying out handles: %v", err)
		}
		eb.BOHandles = uint64(addr)
		eb.NumBOHandles = uint32(len(handles))
	}
	err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_EXECBUFFER, eb.SizeBytes()), &eb)
	return eb.FenceFD, err
}

// mmap maps length bytes of file from offset, shared and writable, as
// mmap(2) does.
func (e *testEnv) mmap(file *vfs.FileDescription, offset, length uint64) (hostarch.Addr, error) {
	e.t.Helper()
	opts := memmap.MMapOpts{
		Length:   length,
		Offset:   offset,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	}
	if err := file.ConfigureMMap(e.task, &opts); err != nil {
		return 0, err
	}
	defer opts.MappingIdentity.DecRef(e.task)
	return e.task.MemoryManager().MMap(e.task, opts)
}

func (e *testEnv) munmap(addr hostarch.Addr, length uint64) {
	e.t.Helper()
	if err := e.task.MemoryManager().MUnmap(e.task, addr, length); err != nil {
		e.t.Fatalf("MUnmap: %v", err)
	}
}

// wantCalls checks the renderer's calls since the last check.
func wantCalls(t *testing.T, r *fakeRenderer, want ...string) {
	t.Helper()
	got := r.takeCalls()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("renderer calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func wantErr(t *testing.T, what string, err error, want *gerrors.Error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s: %v, want success", what, err)
		}
		return
	}
	if !linuxerr.Equals(want, err) {
		t.Errorf("%s: got error %v, want %v", what, err, want)
	}
}

func TestVersion(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	name, date, desc := e.alloc(32), e.alloc(32), e.alloc(32)
	v := linux.DRMVersion{
		NameLen: 32, Name: uint64(name),
		DateLen: 32, Date: uint64(date),
		// Too short: Linux copies what fits, and reports the full length.
		DescLen: 3, Desc: uint64(desc),
	}
	if err := e.ioctl(file, drmIOWR(linux.DRM_NR_VERSION, v.SizeBytes()), &v); err != nil {
		t.Fatalf("DRM_IOCTL_VERSION: %v", err)
	}
	if v.VersionMajor != 0 || v.VersionMinor != 1 || v.VersionPatchlevel != 0 {
		t.Errorf("version %d.%d.%d, want 0.1.0", v.VersionMajor, v.VersionMinor, v.VersionPatchlevel)
	}
	for _, field := range []struct {
		addr hostarch.Addr
		n    uint64
		want string
	}{
		{name, v.NameLen, "virtio_gpu"},
		{date, v.DateLen, "0"},
		{desc, 3, "vir"},
	} {
		if got := string(e.get(field.addr, int(field.n))); got != field.want {
			t.Errorf("got %q, want %q", got, field.want)
		}
	}
	if v.DescLen != uint64(len("virtio GPU")) {
		t.Errorf("description length %d, want %d", v.DescLen, len("virtio GPU"))
	}
	// Other drivers' ioctls are not the device's.
	var args arch.SyscallArguments
	args[1].Value = uintptr(linux.IOWR('x', 0, 8))
	if _, err := file.Ioctl(e.ctx, e.task.MemoryManager(), 0, args); !linuxerr.Equals(linuxerr.ENOTTY, err) {
		t.Errorf("an ioctl of another type: %v, want ENOTTY", err)
	}
}

func TestGetparamAndCaps(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	value := e.alloc(8)
	for _, test := range []struct {
		param uint64
		want  int32
		err   *gerrors.Error
	}{
		{linux.VIRTGPU_PARAM_3D_FEATURES, 1, nil},
		{linux.VIRTGPU_PARAM_CAPSET_QUERY_FIX, 1, nil},
		{linux.VIRTGPU_PARAM_RESOURCE_BLOB, 1, nil},
		{linux.VIRTGPU_PARAM_HOST_VISIBLE, 1, nil},
		{linux.VIRTGPU_PARAM_CROSS_DEVICE, 0, nil},
		{linux.VIRTGPU_PARAM_CONTEXT_INIT, 1, nil},
		{linux.VIRTGPU_PARAM_SUPPORTED_CAPSET_IDS, 1 << linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, nil},
		{linux.VIRTGPU_PARAM_EXPLICIT_DEBUG_NAME, 1, nil},
		{9, 0, linuxerr.EINVAL},
		{10, 0, linuxerr.EINVAL},
	} {
		if _, err := primitive.CopyInt32Out(e.task, value, -1); err != nil {
			t.Fatal(err)
		}
		p := linux.DRMVirtgpuGetparam{Param: test.param, Value: uint64(value)}
		err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_GETPARAM, p.SizeBytes()), &p)
		wantErr(t, fmt.Sprintf("GETPARAM %d", test.param), err, test.err)
		if err != nil {
			continue
		}
		var got int32
		if _, err := primitive.CopyInt32In(e.task, value, &got); err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("GETPARAM %d: %d, want %d", test.param, got, test.want)
		}
	}

	buf := e.alloc(256)
	for _, test := range []struct {
		id, version, size uint32
		want              []byte
		err               *gerrors.Error
	}{
		{linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 1, 256, testCapset, nil},
		{linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 0, 256, testCapset, nil},
		{linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 1, 9, testCapset[:9], nil},
		{linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 2, 256, nil, linuxerr.EINVAL},
		{linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 1, 0, nil, linuxerr.EINVAL},
		{linux.VIRTGPU_DRM_CAPSET_VENUS, 0, 256, nil, linuxerr.EINVAL},
	} {
		if _, err := e.task.CopyOutBytes(buf, make([]byte, 256)); err != nil {
			t.Fatal(err)
		}
		c := linux.DRMVirtgpuGetCaps{CapSetID: test.id, CapSetVer: test.version, Addr: uint64(buf), Size: test.size}
		err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_GET_CAPS, c.SizeBytes()), &c)
		wantErr(t, fmt.Sprintf("GET_CAPS %d version %d size %d", test.id, test.version, test.size), err, test.err)
		if err != nil {
			continue
		}
		got := e.get(buf, 256)
		want := make([]byte, 256)
		copy(want, test.want)
		if !bytes.Equal(got, want) {
			t.Errorf("GET_CAPS %d version %d size %d: %q, want %q", test.id, test.version, test.size, got, want)
		}
	}

	c := linux.DRMGetCap{Capability: linux.DRM_CAP_PRIME}
	if err := e.ioctl(file, drmIOWR(linux.DRM_NR_GET_CAP, c.SizeBytes()), &c); err != nil || c.Value != linux.DRM_PRIME_CAP_IMPORT|linux.DRM_PRIME_CAP_EXPORT {
		t.Errorf("DRM_CAP_PRIME: %d, %v", c.Value, err)
	}
	c = linux.DRMGetCap{Capability: linux.DRM_CAP_SYNCOBJ, Value: 7}
	if err := e.ioctl(file, drmIOWR(linux.DRM_NR_GET_CAP, c.SizeBytes()), &c); err != nil || c.Value != 0 {
		t.Errorf("DRM_CAP_SYNCOBJ: %d, %v", c.Value, err)
	}
	c = linux.DRMGetCap{Capability: 0x1 /* DRM_CAP_DUMB_BUFFER */}
	wantErr(t, "DRM_CAP_DUMB_BUFFER", e.ioctl(file, drmIOWR(linux.DRM_NR_GET_CAP, c.SizeBytes()), &c), linuxerr.EOPNOTSUPP)
	// A render node refuses DRM_IOCTL_GET_MAGIC, as Linux's does.
	var magic primitive.Uint32
	wantErr(t, "DRM_IOCTL_GET_MAGIC", e.ioctl(file, linux.IOR(linux.DRM_IOCTL_BASE, 0x02, 4), &magic), linuxerr.EACCES)
}

func TestContextInit(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if got := e.fakes.count(); got != 0 {
		t.Fatalf("%d renderers started before a context, want 0", got)
	}
	if err := e.contextInit(file, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 2, "vulkaninfo"); err != nil {
		t.Fatalf("CONTEXT_INIT: %v", err)
	}
	r := e.fakes.last(t)
	wantCalls(t, r, `CreateContext 1 capset 3 "vulkaninfo"`)
	wantErr(t, "a second CONTEXT_INIT", e.contextInit(file, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 2, ""), linuxerr.EEXIST)

	for _, test := range []struct {
		name   string
		params []linux.DRMVirtgpuContextSetParam
	}{
		{"an unsupported capability set", []linux.DRMVirtgpuContextSetParam{{Param: linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID, Value: linux.VIRTGPU_DRM_CAPSET_VENUS}}},
		{"a capability set out of range", []linux.DRMVirtgpuContextSetParam{{Param: linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID, Value: 64}}},
		{"two capability sets", []linux.DRMVirtgpuContextSetParam{{Param: linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID, Value: 3}, {Param: linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID, Value: 3}}},
		{"too many rings", []linux.DRMVirtgpuContextSetParam{{Param: linux.VIRTGPU_CONTEXT_PARAM_NUM_RINGS, Value: 65}}},
		{"a poll rings mask", []linux.DRMVirtgpuContextSetParam{{Param: linux.VIRTGPU_CONTEXT_PARAM_POLL_RINGS_MASK, Value: 1}}},
		{"two poll rings masks", []linux.DRMVirtgpuContextSetParam{{Param: linux.VIRTGPU_CONTEXT_PARAM_POLL_RINGS_MASK}, {Param: linux.VIRTGPU_CONTEXT_PARAM_POLL_RINGS_MASK}}},
		{"an unknown parameter", []linux.DRMVirtgpuContextSetParam{{Param: 99, Value: 1}}},
		{"five parameters", make([]linux.DRMVirtgpuContextSetParam, 5)},
	} {
		other := e.open()
		wantErr(t, test.name, e.contextInitParams(other, test.params), linuxerr.EINVAL)
	}
	wantCalls(t, r)

	// Mesa's Venus driver asks for no fence events: a mask of no rings.
	venus := e.open()
	if err := e.contextInitParams(venus, []linux.DRMVirtgpuContextSetParam{
		{Param: linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID, Value: linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN},
		{Param: linux.VIRTGPU_CONTEXT_PARAM_NUM_RINGS, Value: 64},
		{Param: linux.VIRTGPU_CONTEXT_PARAM_POLL_RINGS_MASK, Value: 0},
	}); err != nil {
		t.Errorf("CONTEXT_INIT with a mask of no rings to poll: %v", err)
	}
	r.takeCalls()

	// A file without CONTEXT_INIT gets a default context when it needs one,
	// named after its task.
	lazy := e.open()
	if _, _, err := e.createBlob(lazy, linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, 1, 4096, nil); err != nil {
		t.Fatalf("creating a blob without a context: %v", err)
	}
	calls := r.takeCalls()
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "CreateContext") || !strings.Contains(calls[0], `capset 0 "gpu-test"`) {
		t.Errorf("renderer calls %q, want a default context first", calls)
	}
	wantErr(t, "CONTEXT_INIT after the default context", e.contextInit(lazy, 3, 1, ""), linuxerr.EEXIST)
	if got := e.fakes.count(); got != 1 {
		t.Errorf("%d renderers started, want 1", got)
	}
}

func TestBlobs(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if err := e.contextInit(file, 3, 2, "blobs"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	r.takeCalls()

	const size = 4 * hostarch.PageSize
	handle, resID, err := e.createBlob(file, linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, 7, size, []byte("mkbl"))
	if err != nil {
		t.Fatalf("RESOURCE_CREATE_BLOB: %v", err)
	}
	if handle != 1 || resID != 1 {
		t.Errorf("handle %d, resource %d, want 1 and 1", handle, resID)
	}
	wantCalls(t, r,
		`SubmitCommand 1 "mkbl"`,
		fmt.Sprintf("CreateBlob 1 1 mem 2 flags 1 id 7 size %d", size),
		"AttachResource 1 1")

	ri := linux.DRMVirtgpuResourceInfo{BOHandle: handle}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_RESOURCE_INFO, ri.SizeBytes()), &ri); err != nil {
		t.Fatalf("RESOURCE_INFO: %v", err)
	}
	if ri.Size != size || ri.ResHandle != resID || ri.BlobMem != linux.VIRTGPU_BLOB_MEM_HOST3D {
		t.Errorf("RESOURCE_INFO: %+v", ri)
	}

	m := linux.DRMVirtgpuMap{Handle: handle}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_MAP, m.SizeBytes()), &m); err != nil {
		t.Fatalf("MAP: %v", err)
	}
	if m.Offset != firstMapOffset {
		t.Errorf("MAP offset %#x, want %#x", m.Offset, uint64(firstMapOffset))
	}

	// The renderer's memory is the guest's.
	addr, err := e.mmap(file, m.Offset, size)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	wantCalls(t, r, "MapResource 1")
	mem := r.memory[resID]
	if _, err := e.task.CopyOutBytes(addr+100, []byte("from the guest")); err != nil {
		t.Fatal(err)
	}
	if got := string(mem.read(t, 100, 14)); got != "from the guest" {
		t.Errorf("the renderer reads %q", got)
	}
	mem.write(t, size-8, []byte("renderer"))
	if got := string(e.get(addr+size-8, 8)); got != "renderer" {
		t.Errorf("the guest reads %q", got)
	}

	// Like Linux, the device maps its memory only whole, from the start.
	if _, err := e.mmap(file, m.Offset, size/2); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("mapping half of a blob: %v, want EINVAL", err)
	}
	if _, err := e.mmap(file, m.Offset+hostarch.PageSize, size-hostarch.PageSize); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("mapping a blob from its middle: %v, want EINVAL", err)
	}
	// Nor copy on write.
	opts := memmap.MMapOpts{Length: size, Offset: m.Offset, Private: true, Perms: hostarch.Read, MaxPerms: hostarch.AnyAccess}
	if err := file.ConfigureMMap(e.task, &opts); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("mapping a blob privately: %v, want EINVAL", err)
	}
	if got := file.Readiness(waiter.ReadableEvents | waiter.WritableEvents); got != 0 {
		t.Errorf("the render node is ready for %v without events", got)
	}
	// Another file may not map the blob without a handle for it.
	other := e.open()
	if _, err := e.mmap(other, m.Offset, size); !linuxerr.Equals(linuxerr.EACCES, err) {
		t.Errorf("mapping another file's blob: %v, want EACCES", err)
	}

	// A mapping keeps the resource after its handle is closed.
	gc := linux.DRMGemClose{Handle: handle}
	if err := e.ioctl(file, linux.IOW(linux.DRM_IOCTL_BASE, linux.DRM_NR_GEM_CLOSE, uint32(gc.SizeBytes())), &gc); err != nil {
		t.Fatalf("GEM_CLOSE: %v", err)
	}
	wantCalls(t, r, "DetachResource 1 1")
	wantErr(t, "GEM_CLOSE of a closed handle", e.ioctl(file, linux.IOW(linux.DRM_IOCTL_BASE, linux.DRM_NR_GEM_CLOSE, uint32(gc.SizeBytes())), &gc), linuxerr.EINVAL)
	if got := string(e.get(addr+100, 14)); got != "from the guest" {
		t.Errorf("after GEM_CLOSE, the guest reads %q", got)
	}
	e.munmap(addr, size)
	wantCalls(t, r, "DestroyResource 1")
	if !mem.released {
		t.Error("the resource's memory was not released")
	}

	// A blob's size need not be whole pages, nor the length that maps it,
	// as Mesa's Venus driver's ring buffers are not.
	const oddSize = 3*hostarch.PageSize + 100
	handle, _ = e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, oddSize)
	m = linux.DRMVirtgpuMap{Handle: handle}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_MAP, m.SizeBytes()), &m); err != nil {
		t.Fatalf("MAP: %v", err)
	}
	if addr, err := e.mmap(file, m.Offset, oddSize); err != nil {
		t.Errorf("mapping a blob of %d bytes: %v", oddSize, err)
	} else {
		e.munmap(addr, oddSize)
	}
	r.takeCalls()

	// A blob that is not mappable is not.
	handle, _ = e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_SHAREABLE, size)
	m = linux.DRMVirtgpuMap{Handle: handle}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_MAP, m.SizeBytes()), &m); err != nil {
		t.Fatalf("MAP: %v", err)
	}
	if _, err := e.mmap(file, m.Offset, size); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("mapping a blob that is not mappable: %v, want EINVAL", err)
	}

	for _, test := range []struct {
		name  string
		mem   uint32
		flags uint32
		id    uint64
		size  uint64
		cmd   []byte
	}{
		{"cross-device", linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_FLAG_USE_CROSS_DEVICE, 1, size, nil},
		{"unknown flags", linux.VIRTGPU_BLOB_MEM_HOST3D, 16, 1, size, nil},
		{"a guest handle of the host's memory", linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_FLAG_CREATE_GUEST_HANDLE, 1, size, nil},
		{"unknown memory", 4, 0, 1, size, nil},
		{"a guest blob with an ID", linux.VIRTGPU_BLOB_MEM_GUEST, 0, 1, size, nil},
		{"a guest blob with a command", linux.VIRTGPU_BLOB_MEM_GUEST, 0, 0, size, []byte("abcd")},
		{"a command that is not whole words", linux.VIRTGPU_BLOB_MEM_HOST3D, 0, 1, size, []byte("abc")},
		{"an empty blob", linux.VIRTGPU_BLOB_MEM_HOST3D, 0, 1, 0, nil},
		{"a huge blob", linux.VIRTGPU_BLOB_MEM_HOST3D, 0, 1, 1 << 40, nil},
	} {
		if _, _, err := e.createBlob(file, test.mem, test.flags, test.id, test.size, test.cmd); !linuxerr.Equals(linuxerr.EINVAL, err) {
			t.Errorf("creating %s: %v, want EINVAL", test.name, err)
		}
	}
}

// TestGuestHandleBlobs checks that the device takes blobs of the guest's
// memory with host handles, of IDs, as Android's minigbm, built against
// ChromeOS's headers, asks for them.
func TestGuestHandleBlobs(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if err := e.contextInit(file, 3, 1, "minigbm"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	r.takeCalls()
	const size = 4 * hostarch.PageSize
	flags := uint32(linux.VIRTGPU_BLOB_FLAG_USE_SHAREABLE | linux.VIRTGPU_BLOB_FLAG_CREATE_GUEST_HANDLE)
	_, resID, err := e.createBlob(file, linux.VIRTGPU_BLOB_MEM_GUEST, flags, 3, size, nil)
	if err != nil {
		t.Fatalf("RESOURCE_CREATE_BLOB: %v", err)
	}
	wantCalls(t, r,
		fmt.Sprintf("CreateBlob 1 %d mem 1 flags %d id 3 size %d", resID, flags, size),
		fmt.Sprintf("AttachResource 1 %d", resID))
}

func TestClassicResources(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if err := e.contextInit(file, 3, 1, "classic"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	r.takeCalls()
	rc := linux.DRMVirtgpuResourceCreate{Target: 2, Format: 1, Width: 64, Height: 32, Size: 64 * 32 * 4}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_RESOURCE_CREATE, rc.SizeBytes()), &rc); err != nil {
		t.Fatalf("RESOURCE_CREATE: %v", err)
	}
	wantCalls(t, r, fmt.Sprintf("CreateResource 1 %d 64x32 format 1 size 8192", rc.ResHandle), fmt.Sprintf("AttachResource 1 %d", rc.ResHandle))

	// A classic resource's backing may be mapped in part.
	m := linux.DRMVirtgpuMap{Handle: rc.BOHandle}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_MAP, m.SizeBytes()), &m); err != nil {
		t.Fatalf("MAP: %v", err)
	}
	addr, err := e.mmap(file, m.Offset, hostarch.PageSize)
	if err != nil {
		t.Fatalf("mapping part of a classic resource: %v", err)
	}
	e.munmap(addr, hostarch.PageSize)
	r.takeCalls()

	x := linux.DRMVirtgpu3DTransfer{BOHandle: rc.BOHandle, Box: linux.DRMVirtgpu3DBox{W: 64, H: 32, D: 1}}
	if err := e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_TRANSFER_TO_HOST, x.SizeBytes()), &x); err != nil {
		t.Fatalf("TRANSFER_TO_HOST: %v", err)
	}
	wantCalls(t, r, fmt.Sprintf("Transfer 1 %d to host true box 0,0 64x32", rc.ResHandle), "CreateFence 1 ctx 1 ring false 0")
	// The transfer's fence makes the resource busy.
	w := linux.DRMVirtgpu3DWait{Handle: rc.BOHandle, Flags: linux.VIRTGPU_WAIT_NOWAIT}
	wantErr(t, "WAIT while transferring", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_WAIT, w.SizeBytes()), &w), linuxerr.EBUSY)
	r.signal(t, 1)
	wantErr(t, "WAIT after the transfer", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_WAIT, w.SizeBytes()), &w), nil)

	// Only the renderer's blobs take strides.
	x.Stride = 256
	wantErr(t, "TRANSFER_FROM_HOST with a stride", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_TRANSFER_FROM_HOST, x.SizeBytes()), &x), linuxerr.EINVAL)
	x = linux.DRMVirtgpu3DTransfer{BOHandle: 99}
	wantErr(t, "TRANSFER_FROM_HOST of no resource", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_TRANSFER_FROM_HOST, x.SizeBytes()), &x), linuxerr.ENOENT)
}

func TestExecbufferAndFences(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if err := e.contextInit(file, 3, 2, "fences"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	r.takeCalls()

	// A submission without a fence or resources has no fence.
	if _, err := e.execbuffer(file, 0, 0, -1, []byte("ping")); err != nil {
		t.Fatalf("EXECBUFFER: %v", err)
	}
	wantCalls(t, r, `SubmitCommand 1 "ping"`)

	fenceFD, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 1, -1, []byte("draw"))
	if err != nil {
		t.Fatalf("EXECBUFFER: %v", err)
	}
	wantCalls(t, r, `SubmitCommand 1 "draw"`, "CreateFence 1 ctx 1 ring true 1")
	syncFile := e.file(fenceFD)
	if got := syncFile.Readiness(waiter.ReadableEvents); got != 0 {
		t.Errorf("an unsignaled fence is ready: %v", got)
	}
	info := linux.SyncFileInfo{}
	if err := e.ioctl(syncFile, linux.SYNC_IOC_FILE_INFO, &info); err != nil || info.Status != 0 || info.NumFences != 1 {
		t.Errorf("SYNC_IOC_FILE_INFO before the signal: %+v, %v", info, err)
	}
	r.signal(t, 1)
	if got := syncFile.Readiness(waiter.ReadableEvents); got != waiter.ReadableEvents {
		t.Errorf("a signaled fence is not ready: %v", got)
	}
	fences := e.alloc(80)
	info = linux.SyncFileInfo{NumFences: 1, SyncFenceInfo: uint64(fences)}
	if err := e.ioctl(syncFile, linux.SYNC_IOC_FILE_INFO, &info); err != nil || info.Status != 1 || info.NumFences != 1 {
		t.Errorf("SYNC_IOC_FILE_INFO after the signal: %+v, %v", info, err)
	}
	var fi linux.SyncFenceInfo
	if _, err := fi.CopyIn(e.task, fences); err != nil {
		t.Fatal(err)
	}
	if fi.Status != 1 || !bytes.HasPrefix(fi.DriverName[:], []byte("virtio_gpu\x00")) {
		t.Errorf("sync_fence_info %+v", fi)
	}

	// Submissions are validated.
	for _, test := range []struct {
		name  string
		flags uint32
		ring  uint32
	}{
		{"a ring out of range", linux.VIRTGPU_EXECBUF_RING_IDX, 2},
		{"gfxstream's shareable out flag", 0x10, 0},
	} {
		if _, err := e.execbuffer(file, test.flags, test.ring, -1, []byte("x")); !linuxerr.Equals(linuxerr.EINVAL, err) {
			t.Errorf("submitting with %s: %v, want EINVAL", test.name, err)
		}
	}
	if _, err := e.execbuffer(file, 0, 0, -1, []byte("x"), 42); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Errorf("submitting with no resource: %v, want ENOENT", err)
	}
	r.takeCalls()

	// A submission that uses a resource has a fence on the global timeline,
	// which WAIT waits for.
	handle, _ := e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, hostarch.PageSize)
	r.takeCalls()
	if _, err := e.execbuffer(file, 0, 0, -1, []byte("ctxc"), handle); err != nil {
		t.Fatalf("EXECBUFFER: %v", err)
	}
	wantCalls(t, r, `SubmitCommand 1 "ctxc"`, "CreateFence 2 ctx 1 ring false 0")
	w := linux.DRMVirtgpu3DWait{Handle: handle, Flags: linux.VIRTGPU_WAIT_NOWAIT}
	wantErr(t, "WAIT NOWAIT while busy", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_WAIT, w.SizeBytes()), &w), linuxerr.EBUSY)
	go func() {
		time.Sleep(10 * time.Millisecond)
		r.signal(t, 2)
	}()
	w.Flags = 0
	wantErr(t, "WAIT", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_WAIT, w.SizeBytes()), &w), nil)
	w.Flags = linux.VIRTGPU_WAIT_NOWAIT
	wantErr(t, "WAIT NOWAIT when idle", e.ioctl(file, virtgpuIOWR(linux.DRM_VIRTGPU_WAIT, w.SizeBytes()), &w), nil)

	// A fence in from another timeline delays the submission until it is
	// signaled; one from the same timeline does not.
	first, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 0, -1, []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	r.takeCalls()
	if _, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_IN, 0, first, []byte("same")); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, `SubmitCommand 1 "same"`)
	done := make(chan error)
	go func() {
		_, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_IN, 1, first, []byte("two"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a submission did not wait for its fence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	wantCalls(t, r)
	r.signal(t, 3)
	if err := <-done; err != nil {
		t.Fatalf("EXECBUFFER after its fence: %v", err)
	}
	wantCalls(t, r, `SubmitCommand 1 "two"`)
	if _, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_FENCE_FD_IN, 0, int32(99), []byte("x")); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("submitting with a fence that is not a sync_file: %v, want EINVAL", err)
	}
}

func TestSyncFileMerge(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if err := e.contextInit(file, 3, 2, "merge"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	flags := uint32(linux.VIRTGPU_EXECBUF_RING_IDX | linux.VIRTGPU_EXECBUF_FENCE_FD_OUT)
	a, err := e.execbuffer(file, flags, 0, -1, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.execbuffer(file, flags, 1, -1, []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	data := linux.SyncMergeData{FD2: b}
	copy(data.Name[:], "both")
	if err := e.ioctl(e.file(a), linux.SYNC_IOC_MERGE, &data); err != nil {
		t.Fatalf("SYNC_IOC_MERGE: %v", err)
	}
	merged := e.file(data.Fence)
	r.signal(t, 1)
	if got := merged.Readiness(waiter.ReadableEvents); got != 0 {
		t.Errorf("a merge of a signaled and an unsignaled fence is ready: %v", got)
	}
	r.signal(t, 2)
	if got := merged.Readiness(waiter.ReadableEvents); got != waiter.ReadableEvents {
		t.Errorf("a merge of signaled fences is not ready: %v", got)
	}
	info := linux.SyncFileInfo{}
	if err := e.ioctl(merged, linux.SYNC_IOC_FILE_INFO, &info); err != nil {
		t.Fatalf("SYNC_IOC_FILE_INFO: %v", err)
	}
	if info.NumFences != 2 || info.Status != 1 || !bytes.HasPrefix(info.Name[:], []byte("both\x00")) {
		t.Errorf("SYNC_IOC_FILE_INFO: %+v", info)
	}
	// Merging with a file that is not a sync_file fails.
	data = linux.SyncMergeData{FD2: 12345}
	wantErr(t, "merging with no file", e.ioctl(e.file(a), linux.SYNC_IOC_MERGE, &data), linuxerr.ENOENT)
}

func TestPrime(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	a, b := e.open(), e.open()
	for _, file := range []*vfs.FileDescription{a, b} {
		if err := e.contextInit(file, 3, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	r := e.fakes.last(t)
	const size = 2 * hostarch.PageSize
	handle, resID := e.mustCreateBlob(a, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE|linux.VIRTGPU_BLOB_FLAG_USE_SHAREABLE, size)
	r.takeCalls()

	export := func(file *vfs.FileDescription, handle, flags uint32) (int32, error) {
		p := linux.DRMPrimeHandle{Handle: handle, Flags: flags}
		err := e.ioctl(file, drmIOWR(linux.DRM_NR_PRIME_HANDLE_TO_FD, p.SizeBytes()), &p)
		return p.FD, err
	}
	imp := func(file *vfs.FileDescription, fd int32) (uint32, error) {
		p := linux.DRMPrimeHandle{FD: fd}
		err := e.ioctl(file, drmIOWR(linux.DRM_NR_PRIME_FD_TO_HANDLE, p.SizeBytes()), &p)
		return p.Handle, err
	}

	fd1, err := export(a, handle, linux.DRM_CLOEXEC|linux.DRM_RDWR)
	if err != nil {
		t.Fatalf("PRIME_HANDLE_TO_FD: %v", err)
	}
	fd2, err := export(a, handle, linux.DRM_CLOEXEC)
	if err != nil {
		t.Fatalf("PRIME_HANDLE_TO_FD: %v", err)
	}
	// The test drops these references itself.
	dmabuf := e.task.GetFile(fd1)
	if again := e.task.GetFile(fd2); again != dmabuf {
		t.Error("exporting a resource twice made two dma-bufs")
	}
	if end, err := dmabuf.Seek(e.task, 0, linux.SEEK_END); err != nil || end != size {
		t.Errorf("lseek(SEEK_END) of a dma-buf: %d, %v; want %d", end, err, size)
	}
	if _, err := export(a, handle, 0x8); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("exporting with unknown flags: %v, want EINVAL", err)
	}

	// Importing into the exporter returns its handle; into another file, a
	// new handle, the same each time.
	if got, err := imp(a, fd1); err != nil || got != handle {
		t.Errorf("importing into the exporter: handle %d, %v; want %d", got, err, handle)
	}
	bHandle, err := imp(b, fd1)
	if err != nil {
		t.Fatalf("PRIME_FD_TO_HANDLE: %v", err)
	}
	if got, err := imp(b, fd2); err != nil || got != bHandle {
		t.Errorf("importing again: handle %d, %v; want %d", got, err, bHandle)
	}
	wantCalls(t, r, fmt.Sprintf("AttachResource 2 %d", resID))
	ri := linux.DRMVirtgpuResourceInfo{BOHandle: bHandle}
	if err := e.ioctl(b, virtgpuIOWR(linux.DRM_VIRTGPU_RESOURCE_INFO, ri.SizeBytes()), &ri); err != nil || ri.ResHandle != resID {
		t.Errorf("RESOURCE_INFO of the import: %+v, %v", ri, err)
	}
	if _, err := imp(b, 12345); !linuxerr.Equals(linuxerr.EBADF, err) {
		t.Errorf("importing no file: %v, want EBADF", err)
	}

	// The dma-buf maps the resource.
	addr, err := e.mmap(dmabuf, 0, size)
	if err != nil {
		t.Fatalf("mapping a dma-buf: %v", err)
	}
	if _, err := e.task.CopyOutBytes(addr, []byte("shared")); err != nil {
		t.Fatal(err)
	}
	if got := string(r.memory[resID].read(t, 0, 6)); got != "shared" {
		t.Errorf("the renderer reads %q through the dma-buf", got)
	}
	e.munmap(addr, size)
	wantCalls(t, r, fmt.Sprintf("MapResource %d", resID))

	// The resource lives until its handles and dma-buf are gone.
	gc := linux.DRMGemClose{Handle: handle}
	if err := e.ioctl(a, linux.IOW(linux.DRM_IOCTL_BASE, linux.DRM_NR_GEM_CLOSE, 8), &gc); err != nil {
		t.Fatal(err)
	}
	gc = linux.DRMGemClose{Handle: bHandle}
	if err := e.ioctl(b, linux.IOW(linux.DRM_IOCTL_BASE, linux.DRM_NR_GEM_CLOSE, 8), &gc); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, r, fmt.Sprintf("DetachResource 1 %d", resID), fmt.Sprintf("DetachResource 2 %d", resID))
	e.closeFD(fd1)
	e.closeFD(fd2)
	// The test's own reference to the dma-buf is the last.
	wantCalls(t, r)
	dmabuf.DecRef(e.task)
	dmabuf.DecRef(e.task)
	wantCalls(t, r, fmt.Sprintf("DestroyResource %d", resID))
}

func TestRendererLost(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file := e.open()
	if err := e.contextInit(file, 3, 1, "lost"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	fenceFD, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 0, -1, []byte("work"))
	if err != nil {
		t.Fatal(err)
	}
	syncFile := e.file(fenceFD)
	r.events.RendererLost(errors.New("the renderer crashed"))
	if got := syncFile.Readiness(waiter.ReadableEvents); got != waiter.ReadableEvents {
		t.Errorf("a fence of a lost renderer is not signaled: %v", got)
	}
	info := linux.SyncFileInfo{}
	if err := e.ioctl(syncFile, linux.SYNC_IOC_FILE_INFO, &info); err != nil || info.Status != -int32(linuxerr.ENODEV.Errno()) {
		t.Errorf("the status of a fence of a lost renderer: %+v, %v", info, err)
	}
	if _, err := e.execbuffer(file, 0, 0, -1, []byte("more")); !linuxerr.Equals(linuxerr.ENODEV, err) {
		t.Errorf("submitting to a lost context: %v, want ENODEV", err)
	}

	// A new context gets a new renderer.
	other := e.open()
	if err := e.contextInit(other, 3, 1, "again"); err != nil {
		t.Fatalf("creating a context after the renderer was lost: %v", err)
	}
	if got := e.fakes.count(); got != 2 {
		t.Fatalf("%d renderers started, want 2", got)
	}
	if !r.closed {
		t.Error("the lost renderer was not closed")
	}
	next := e.fakes.last(t)
	wantCalls(t, next, `CreateContext 2 capset 3 "again"`)
	// Events of the lost renderer are ignored.
	r.events.FenceSignaled(Fence{ID: 1, CtxID: 1, HasRing: true})
}

func TestRelease(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	file, err := e.k.VFS().OpenAt(e.task, e.creds, pathOp(e.root, "/dev/"+renderNodePath), &vfs.OpenOptions{Flags: linux.O_RDWR})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.contextInit(file, 3, 1, "release"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	_, resID := e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, hostarch.PageSize)
	fenceFD, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 0, -1, []byte("last"))
	if err != nil {
		t.Fatal(err)
	}
	syncFile := e.file(fenceFD)
	r.takeCalls()
	file.DecRef(e.task)
	wantCalls(t, r, fmt.Sprintf("DetachResource 1 %d", resID), fmt.Sprintf("DestroyResource %d", resID), "DestroyContext 1")
	// The renderer signals no more fences of a destroyed context.
	info := linux.SyncFileInfo{}
	if err := e.ioctl(syncFile, linux.SYNC_IOC_FILE_INFO, &info); err != nil || info.Status != -int32(linuxerr.ECANCELED.Errno()) {
		t.Errorf("the status of a fence of a destroyed context: %+v, %v", info, err)
	}
	// Nothing uses the renderer, so the device closed it.
	if !r.isClosed() {
		t.Error("the idle renderer is open")
	}
	// The context ID is free again, and the next context has a new renderer.
	next := e.open()
	if err := e.contextInit(next, 3, 1, "next"); err != nil {
		t.Fatal(err)
	}
	if e.fakes.last(t) == r {
		t.Fatal("the next context has the closed renderer")
	}
	wantCalls(t, e.fakes.last(t), `CreateContext 1 capset 3 "next"`)
}

// TestIdleRenderer checks that the device keeps its renderer while a context
// or a resource of it is left, and no longer.
func TestIdleRenderer(t *testing.T) {
	e := newTestEnv(t, ForkIneligible)
	// The test releases the file itself.
	file, err := e.k.VFS().OpenAt(e.task, e.creds, pathOp(e.root, "/dev/"+renderNodePath), &vfs.OpenOptions{Flags: linux.O_RDWR})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.contextInit(file, 3, 1, "idle"); err != nil {
		t.Fatal(err)
	}
	r := e.fakes.last(t)
	handle, _ := e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, hostarch.PageSize)
	addr, err := e.mmap(file, file.Impl().(*renderFD).objects[handle].mapOffset, hostarch.PageSize)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	// A fence on the global timeline, which outlives its context.
	fenceFD, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 0, -1, []byte("global"))
	if err != nil {
		t.Fatal(err)
	}
	syncFile := e.file(fenceFD)
	file.DecRef(e.task)
	if r.isClosed() {
		t.Fatal("the renderer was closed while a mapping kept a resource")
	}
	e.munmap(addr, hostarch.PageSize)
	if !r.isClosed() {
		t.Fatal("the renderer is open after its last resource was destroyed")
	}
	info := linux.SyncFileInfo{}
	if err := e.ioctl(syncFile, linux.SYNC_IOC_FILE_INFO, &info); err != nil || info.Status != -int32(linuxerr.ECANCELED.Errno()) {
		t.Errorf("the status of a global fence of a closed renderer: %+v, %v", info, err)
	}
	// Late events of the closed renderer are ignored.
	r.events.RendererLost(errors.New("exited"))
	next := e.open()
	if err := e.contextInit(next, 3, 1, "next"); err != nil {
		t.Fatalf("creating a context after the renderer was closed: %v", err)
	}
	if e.fakes.count() != 2 {
		t.Errorf("%d renderers were started, want 2", e.fakes.count())
	}
}

func TestLiveForkValidation(t *testing.T) {
	for _, policy := range []ForkPolicy{ForkIneligible, ForkDeviceLost, ForkSnapshot} {
		t.Run(policy.String(), func(t *testing.T) {
			e := newTestEnv(t, policy)
			file := e.open()
			fd := file.Impl().(*renderFD)
			if _, err := fd.ValidateLiveFork(); err != nil {
				t.Errorf("an unused file refuses a fork: %v", err)
			}
			if err := e.contextInit(file, 3, 1, ""); err != nil {
				t.Fatal(err)
			}
			_, err := fd.ValidateLiveFork()
			if (err == nil) != (policy != ForkIneligible) {
				t.Errorf("a file with a context: %v", err)
			}
			handle, _ := e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, hostarch.PageSize)
			res := fd.objects[handle]
			if err := res.ValidateLiveForkMapping(); (err == nil) != (policy != ForkIneligible) {
				t.Errorf("a mapping: %v", err)
			}
			fenceFD, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 0, -1, []byte("x"))
			if err != nil {
				t.Fatal(err)
			}
			sf := e.file(fenceFD).Impl().(*syncFile)
			if _, err := sf.ValidateLiveFork(); (err == nil) != (policy != ForkIneligible) {
				t.Errorf("an unsignaled sync_file: %v", err)
			}
			e.fakes.last(t).signal(t, 1)
			if _, err := sf.ValidateLiveFork(); err != nil {
				t.Errorf("a signaled sync_file refuses a fork: %v", err)
			}
		})
	}
}

func TestSaveAndLoad(t *testing.T) {
	e := newTestEnv(t, ForkDeviceLost)
	file := e.open()
	if err := e.contextInit(file, 3, 1, "saved"); err != nil {
		t.Fatal(err)
	}
	handle, _ := e.mustCreateBlob(file, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, hostarch.PageSize)
	if _, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX, 0, -1, []byte("x"), handle); err != nil {
		t.Fatal(err)
	}
	d := file.Impl().(*renderFD).dev
	child := loadDevice(t, d, e.fakes.newRenderer)
	if child.gen != d.gen+1 {
		t.Errorf("the loaded device's generation is %d, want %d", child.gen, d.gen+1)
	}
	if len(child.timelines) != 0 {
		t.Errorf("the loaded device has pending fences on %d timelines", len(child.timelines))
	}
	if len(child.resources) != 1 {
		t.Fatalf("the loaded device has %d resources, want 1", len(child.resources))
	}
	for _, r := range child.resources {
		if r.memory != nil || len(r.fences) != 0 {
			t.Errorf("a loaded resource has memory %v and fences %v", r.memory, r.fences)
		}
		// Its memory was the lost renderer's, so accesses to it fault.
		if err := translateAll(r); !isBusError(err) {
			t.Errorf("translating a loaded resource: %v, want a bus error", err)
		}
	}
	if child.newRenderer == nil {
		t.Error("the loaded device has no way to start a renderer")
	}
}

// loadDevice saves d and loads a copy of it, as a fork does.
func loadDevice(t *testing.T, d *device, newRenderer NewRendererFunc) *device {
	t.Helper()
	return loadSavedDevice(t, saveDevice(t, d), newRenderer)
}

// saveDevice saves d, as a fork does.
func saveDevice(t *testing.T, d *device) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, d); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	return buf.Bytes()
}

// loadSavedDevice loads a copy of a device that saveDevice saved.
func loadSavedDevice(t *testing.T, saved []byte, newRenderer NewRendererFunc) *device {
	t.Helper()
	child := &device{}
	ctx := context.WithValue(context.Background(), CtxNewRenderer, newRenderer)
	if _, err := state.Load(ctx, bytes.NewReader(saved), child); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return child
}

// readTranslation reads n bytes at offset of what a resource translates to.
func readTranslation(t *testing.T, r *resource, offset, n uint64) []byte {
	t.Helper()
	whole := memmap.MappableRange{Start: 0, End: r.size}
	ts, err := r.Translate(context.Background(), whole, whole, hostarch.Read)
	if err != nil || len(ts) != 1 {
		t.Fatalf("Translate: %v, %v", ts, err)
	}
	fr := memmap.FileRange{Start: ts[0].Offset + offset, End: ts[0].Offset + offset + n}
	ims, err := ts[0].File.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	buf := make([]byte, n)
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), ims); err != nil {
		t.Fatal(err)
	}
	return buf
}

// translateAll translates the whole of a resource.
func translateAll(r *resource) error {
	whole := memmap.MappableRange{Start: 0, End: r.size}
	_, err := r.Translate(context.Background(), whole, whole, hostarch.Read)
	return err
}

// isBusError returns whether err makes a fault raise SIGBUS.
func isBusError(err error) bool {
	var busErr *memmap.BusError
	return errors.As(err, &busErr)
}

// TestDeviceLostFork loads a device as a fork's child with ForkDeviceLost
// does: the child's mappings of GPU memory fault, and the source's still map
// the renderer's memory.
func TestDeviceLostFork(t *testing.T) {
	e := newTestEnv(t, ForkDeviceLost)
	file := e.open()
	if err := e.contextInit(file, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 2, "lost"); err != nil {
		t.Fatal(err)
	}
	const size = 64 << 10
	handle, _, err := e.createBlob(file, linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, 0, size, nil)
	if err != nil {
		t.Fatal(err)
	}
	fd := file.Impl().(*renderFD)
	r := fd.objects[handle]
	addr, err := e.mmap(file, r.mapOffset, size)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	if _, err := e.task.CopyOutBytes(addr, []byte("guest")); err != nil {
		t.Fatal(err)
	}
	// A fork invalidates the translations, which it cannot save; the
	// source's mapping translates again.
	if err := r.InvalidateUnsavable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(e.get(addr, 5)); got != "guest" {
		t.Errorf("after invalidation, the guest reads %q", got)
	}
	// This test saves only the device, which a mapping would tie to the
	// whole memory manager.
	e.munmap(addr, size)

	child := loadDevice(t, fd.dev, e.fakes.newRenderer)
	if err := translateAll(child.resources[r.id]); !isBusError(err) {
		t.Errorf("translating the child's blob: %v, want a bus error", err)
	}
	if got := string(readTranslation(t, r, 0, 5)); got != "guest" {
		t.Errorf("the source's blob is %q", got)
	}
}

// forkWithSnapshot prepares d's save, as a fork with ForkSnapshot does,
// loads a copy of it, and ends the save.
func forkWithSnapshot(t *testing.T, e *testEnv, d *device) *device {
	t.Helper()
	if err := d.PrepareSave(context.Background()); err != nil {
		t.Fatalf("PrepareSave: %v", err)
	}
	child := loadDevice(t, d, e.fakes.newRenderer)
	d.BeforeResume(context.Background())
	if d.snapshot != nil {
		t.Error("the source kept its snapshot after the save")
	}
	return child
}

// TestSnapshotFork forks a device with ForkSnapshot: the child's contexts
// and resources survive, in a renderer that restores the source's snapshot
// on the child's first use, and its mappings map what the guest wrote.
func TestSnapshotFork(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	file := e.open()
	if err := e.contextInit(file, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 1, "snap"); err != nil {
		t.Fatal(err)
	}
	const size = 64 << 10
	handle, _, err := e.createBlob(file, linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE, 0, size, nil)
	if err != nil {
		t.Fatal(err)
	}
	fd := file.Impl().(*renderFD)
	r := fd.objects[handle]
	addr, err := e.mmap(file, r.mapOffset, size)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	if _, err := e.task.CopyOutBytes(addr, []byte("before the fork")); err != nil {
		t.Fatal(err)
	}
	source := e.fakes.last(t)
	if err := r.InvalidateUnsavable(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.munmap(addr, size)
	source.takeCalls()

	child := forkWithSnapshot(t, e, fd.dev)
	wantCalls(t, source, "Snapshot")
	if child.gen != fd.dev.gen || child.contexts != 1 || child.genResources != 1 {
		t.Errorf("the child has generation %d, %d contexts, and %d resources; want %d, 1, and 1", child.gen, child.contexts, child.genResources, fd.dev.gen)
	}
	if e.fakes.count() != 1 {
		t.Fatalf("the fork started %d renderers, want none", e.fakes.count()-1)
	}
	// The child's first use of the GPU, a fault on its mapping, restores
	// the snapshot in a renderer of its own.
	if got := string(readTranslation(t, child.resources[r.id], 0, 15)); got != "before the fork" {
		t.Errorf("the child's blob holds %q", got)
	}
	restored := e.fakes.last(t)
	if restored == source {
		t.Fatal("the child uses the source's renderer")
	}
	wantCalls(t, restored, fmt.Sprintf("Restore [%d]", r.id), fmt.Sprintf("MapResource %d", r.id))
	// The source's memory is its renderer's.
	if got := string(readTranslation(t, r, 0, 15)); got != "before the fork" {
		t.Errorf("the source's blob holds %q", got)
	}
}

// TestReferenceSnapshotFork forks a device whose renderer's snapshots are
// references, as the Venus renderer's are: the child restores the snapshot
// as soon as the source's save ends, as the last of its holders.
func TestReferenceSnapshotFork(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	e.fakes.referenceSnapshots = true
	file := e.open()
	if err := e.contextInit(file, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, 1, "snap"); err != nil {
		t.Fatal(err)
	}
	fd := file.Impl().(*renderFD)
	source := e.fakes.last(t)
	source.takeCalls()
	child := forkWithSnapshot(t, e, fd.dev)
	<-child.restoredEagerly
	wantCalls(t, source, "Snapshot")
	restored := e.fakes.last(t)
	if restored == source {
		t.Fatal("the child did not restore the snapshot")
	}
	wantCalls(t, restored, "Restore []")
	if child.snapshot != nil || child.renderer != restored {
		t.Errorf("the child kept its snapshot, or has no renderer")
	}
}

// TestReferenceSnapshotFanOut checks that of the children that load one
// save, each but the last restores the snapshot as one of several, and the
// last as the last.
func TestReferenceSnapshotFanOut(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	e.fakes.referenceSnapshots = true
	file := e.open()
	if err := e.contextInit(file, 3, 1, "snap"); err != nil {
		t.Fatal(err)
	}
	d := file.Impl().(*renderFD).dev
	if err := d.PrepareSave(context.Background()); err != nil {
		t.Fatalf("PrepareSave: %v", err)
	}
	one := loadDevice(t, d, e.fakes.newRenderer)
	two := loadDevice(t, d, e.fakes.newRenderer)
	d.BeforeResume(context.Background())
	<-one.restoredEagerly
	<-two.restoredEagerly
	var restores []string
	e.fakes.mu.Lock()
	for _, r := range e.fakes.renderers[1:] {
		restores = append(restores, r.takeCalls()...)
	}
	e.fakes.mu.Unlock()
	slices.Sort(restores)
	if want := []string{"Restore []", "Restore [], not the last"}; !slices.Equal(restores, want) {
		t.Errorf("the children's restores are %q, want %q", restores, want)
	}
}

// TestReferenceSnapshotBatchSiblings checks that the source of a save whose
// loads follow it, as a live fork's batch siblings load what the save of
// their primary sibling captured, holds its reference snapshot until the
// loads end.
func TestReferenceSnapshotBatchSiblings(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	e.fakes.referenceSnapshots = true
	file := e.open()
	if err := e.contextInit(file, 3, 1, "snap"); err != nil {
		t.Fatal(err)
	}
	d := file.Impl().(*renderFD).dev
	source := e.fakes.last(t)
	source.takeCalls()
	loads := &vfs.LiveForkLoads{}
	ctx := context.WithValue(context.Background(), vfs.CtxLiveForkLoads, loads)
	if err := d.PrepareSave(ctx); err != nil {
		t.Fatalf("PrepareSave: %v", err)
	}
	saved := saveDevice(t, d)
	d.BeforeResume(ctx)
	one := loadSavedDevice(t, saved, e.fakes.newRenderer)
	two := loadSavedDevice(t, saved, e.fakes.newRenderer)
	loads.Done()
	<-one.restoredEagerly
	<-two.restoredEagerly
	wantCalls(t, source, "Snapshot")
	var restores []string
	e.fakes.mu.Lock()
	for _, r := range e.fakes.renderers[1:] {
		restores = append(restores, r.takeCalls()...)
	}
	e.fakes.mu.Unlock()
	slices.Sort(restores)
	if want := []string{"Restore []", "Restore [], not the last"}; !slices.Equal(restores, want) {
		t.Errorf("the siblings' restores are %q, want %q", restores, want)
	}
}

// TestReferenceSnapshotSaveBeforeRestore checks that a loaded device that
// has yet to restore its reference snapshot restores it before a save takes
// a snapshot of its own, which a load of the save restores.
func TestReferenceSnapshotSaveBeforeRestore(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	e.fakes.referenceSnapshots = true
	file := e.open()
	if err := e.contextInit(file, 3, 1, "snap"); err != nil {
		t.Fatal(err)
	}
	d := file.Impl().(*renderFD).dev
	source := e.fakes.last(t)
	source.takeCalls()
	if err := d.PrepareSave(context.Background()); err != nil {
		t.Fatalf("PrepareSave: %v", err)
	}
	// The source holds its snapshot until its save ends, which keeps the
	// child from restoring it eagerly.
	child := loadDevice(t, d, e.fakes.newRenderer)
	if err := child.PrepareSave(context.Background()); err != nil {
		t.Fatalf("PrepareSave of the child: %v", err)
	}
	restored := e.fakes.last(t)
	if restored == source {
		t.Fatal("the child's save did not restore the child's snapshot")
	}
	grandchild := loadDevice(t, child, e.fakes.newRenderer)
	child.BeforeResume(context.Background())
	d.BeforeResume(context.Background())
	<-child.restoredEagerly
	<-grandchild.restoredEagerly
	wantCalls(t, source, "Snapshot", "ForgetSnapshot")
	wantCalls(t, restored, "Restore [], not the last", "Snapshot")
	wantCalls(t, e.fakes.last(t), "Restore []")
}

// TestReferenceSnapshotWithoutChildren checks that a save that loads no
// device, such as a checkpoint, tells the renderer to forget its reference
// snapshot.
func TestReferenceSnapshotWithoutChildren(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	e.fakes.referenceSnapshots = true
	file := e.open()
	if err := e.contextInit(file, 3, 1, "snap"); err != nil {
		t.Fatal(err)
	}
	d := file.Impl().(*renderFD).dev
	source := e.fakes.last(t)
	source.takeCalls()
	if err := d.PrepareSave(context.Background()); err != nil {
		t.Fatalf("PrepareSave: %v", err)
	}
	d.BeforeResume(context.Background())
	wantCalls(t, source, "Snapshot", "ForgetSnapshot")
}

// TestSnapshotForkWaitsForFences checks that a save waits for the renderer's
// pending fences, and fails if they stay pending.
func TestSnapshotForkWaitsForFences(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	file := e.open()
	if err := e.contextInit(file, 3, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.execbuffer(file, linux.VIRTGPU_EXECBUF_RING_IDX|linux.VIRTGPU_EXECBUF_FENCE_FD_OUT, 0, -1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	d := file.Impl().(*renderFD).dev
	d.submitMu.Lock()
	err := d.waitForFencesLocked(20 * time.Millisecond)
	d.submitMu.Unlock()
	if err == nil {
		t.Error("the save did not wait for a pending fence")
	}
	e.fakes.last(t).signal(t, 1)
	if err := d.PrepareSave(context.Background()); err != nil {
		t.Errorf("PrepareSave with no fence pending: %v", err)
	}
	d.BeforeResume(context.Background())
}

// TestSnapshotRestoreFails checks that a child whose renderer fails to
// restore the snapshot loses the snapshot's contexts, and makes new ones.
func TestSnapshotRestoreFails(t *testing.T) {
	e := newTestEnv(t, ForkSnapshot)
	file := e.open()
	if err := e.contextInit(file, 3, 1, ""); err != nil {
		t.Fatal(err)
	}
	fd := file.Impl().(*renderFD)
	child := forkWithSnapshot(t, e, fd.dev)
	e.fakes.mu.Lock()
	e.fakes.failRestore = true
	e.fakes.mu.Unlock()
	child.submitMu.Lock()
	_, err := child.rendererLocked(child.gen)
	child.submitMu.Unlock()
	if !linuxerr.Equals(linuxerr.ENODEV, err) {
		t.Errorf("using a context of the failed snapshot: %v, want ENODEV", err)
	}
	if child.contexts != 0 || child.snapshot != nil {
		t.Errorf("after a failed restore, the child has %d contexts, and a snapshot of %d bytes", child.contexts, len(child.snapshot))
	}
	child.submitMu.Lock()
	_, err = child.startRendererLocked()
	child.submitMu.Unlock()
	if err != nil {
		t.Errorf("starting a renderer after a failed restore: %v", err)
	}
}

func TestIDAllocator(t *testing.T) {
	a := idAllocator{max: 5}
	var got []uint32
	for i := 0; i < 5; i++ {
		id, ok := a.alloc()
		if !ok {
			t.Fatalf("alloc %d failed", i)
		}
		got = append(got, id)
	}
	if fmt.Sprint(got) != "[1 2 3 4 5]" {
		t.Errorf("allocated %v", got)
	}
	if _, ok := a.alloc(); ok {
		t.Error("allocated past the maximum")
	}
	a.release(4)
	a.release(2)
	a.release(5)
	got = nil
	for i := 0; i < 3; i++ {
		id, _ := a.alloc()
		got = append(got, id)
	}
	if fmt.Sprint(got) != "[2 4 5]" {
		t.Errorf("reallocated %v, want the lowest free IDs", got)
	}
}

// chanBlocker blocks without a task.
type chanBlocker struct {
	context.Context
}

// BlockWithTimeout implements context.Context.BlockWithTimeout.
func (chanBlocker) BlockWithTimeout(ch chan struct{}, haveTimeout bool, timeout time.Duration) (time.Duration, error) {
	return testBlocker{}.BlockWithTimeout(ch, haveTimeout, timeout)
}

// TestConcurrentFences creates, signals, merges, and waits for fences from
// many goroutines, for the race detector: unlike the other tests, it needs no
// task, whose methods race builds allow only on the task's goroutine.
func TestConcurrentFences(t *testing.T) {
	fakes := &fakeRenderers{}
	d, err := newDevice(Options{
		Capsets:     []Capset{{ID: linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, MaxVersion: 1, Data: testCapset}},
		NewRenderer: fakes.newRenderer,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.submitMu.Lock()
	if _, err := d.startRendererLocked(); err != nil {
		t.Fatal(err)
	}
	renderer := d.renderer
	d.submitMu.Unlock()
	fake := fakes.last(t)
	res := &resource{dev: d, id: 1}
	res.InitRefs()

	const perTimeline = 300
	const timelines = 3
	ctx := chanBlocker{context.Background()}
	created := make(chan *fence, timelines*perTimeline)
	var creators, waiters sync.WaitGroup
	for tl := 0; tl < timelines; tl++ {
		creators.Add(1)
		go func(tl int) {
			defer creators.Done()
			for i := 0; i < perTimeline; i++ {
				d.submitMu.Lock()
				// Two rings of context 1, and the global timeline.
				f := d.createFenceLocked(renderer, 1, tl < 2, uint32(tl))
				d.submitMu.Unlock()
				d.mu.Lock()
				res.addFenceLocked(f)
				d.mu.Unlock()
				created <- f
			}
		}(tl)
	}
	stop := make(chan struct{})
	signaled := make(chan struct{})
	go func() {
		defer close(signaled)
		next := 0
		for {
			fake.mu.Lock()
			fences := append([]Fence(nil), fake.fences[next:]...)
			fake.mu.Unlock()
			// Signal the newest fence of the batch first: it signals the
			// older ones on its timeline too.
			for i := len(fences) - 1; i >= 0; i-- {
				fake.events.FenceSignaled(fences[i])
			}
			next += len(fences)
			select {
			case <-stop:
				if next == timelines*perTimeline {
					return
				}
			default:
			}
		}
	}()
	errs := make(chan error, timelines*perTimeline)
	for i := 0; i < 4; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			var prev *fence
			for f := range created {
				if err := d.waitFence(ctx, f, true, 10*time.Second); err != nil {
					errs <- fmt.Errorf("waiting for fence %d: %w", f.id, err)
				}
				if prev != nil {
					d.mu.Lock()
					merged := mergeFencesLocked(prev, f)
					d.mu.Unlock()
					if err := d.waitFence(ctx, merged, true, 10*time.Second); err != nil {
						errs <- fmt.Errorf("waiting for a merge: %w", err)
					}
				}
				prev = f
			}
		}()
	}
	creators.Wait()
	close(created)
	waiters.Wait()
	close(stop)
	<-signaled
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if err := d.waitIdle(ctx, res, 10*time.Second); err != nil {
		t.Errorf("waiting for the resource: %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.timelines) != 0 || len(res.fences) != 0 {
		t.Errorf("%d timelines and %d resource fences remain", len(d.timelines), len(res.fences))
	}
}
