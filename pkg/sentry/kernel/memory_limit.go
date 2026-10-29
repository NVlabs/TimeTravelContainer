// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// MemoryLimit is the memory limit of the host cgroup that holds the Sentry and
// its application processes; see SetMemoryLimit.
type MemoryLimit struct {
	// Total is the most memory and swap the cgroup may hold, or
	// math.MaxUint64 if its swap is unlimited.
	Total uint64

	// Memory is the most memory the cgroup may hold, swap aside.
	Memory uint64

	// Current, Stat and SwapCurrent are the cgroup's memory.current,
	// memory.stat and memory.swap.current files. SwapCurrent is nil if Total
	// limits memory alone.
	Current, Stat, SwapCurrent *os.File

	// HostMeminfo is the host's /proc/meminfo, or nil. With it, a cgroup that
	// may swap holds no more than the swap free on the host lets it; see
	// memoryLimiter.total.
	HostMeminfo *os.File
}

const (
	// memoryCheckInterval is how often a family over no more than its limit
	// checks what its cgroup holds, which may grow without its application
	// memory, as the Sentry's own does.
	memoryCheckInterval = 100 * time.Millisecond

	// memoryKillPollInterval is how often a family over its limit checks
	// whether it is within it again.
	memoryKillPollInterval = 2 * time.Millisecond

	// memoryKillGrace is how long a family over its limit waits for what it
	// holds to shrink, once nothing frees memory any more, before it kills
	// another process: the processes it killed for it have exited, and the
	// MemoryFile has released what they freed.
	memoryKillGrace = 250 * time.Millisecond

	// memoryKillPatience is how long a family over its limit waits for what
	// it holds to shrink while memory is being freed, before it kills another
	// process anyway: the processes it killed for it exit, or the MemoryFile
	// releases what was freed. The Sentry frees memory slowly when the host
	// has swapped its own out, as a cgroup that is full does: a process it
	// killed took 0.2 s to exit.
	memoryKillPatience = 2 * time.Second

	// memoryProgress is how much what a family holds must shrink to show
	// that its memory is being freed.
	memoryProgress = 1 << 20

	// oomScoreAdjMin is the oom_score_adj of a process its out-of-memory
	// killer never kills, OOM_SCORE_ADJ_MIN.
	oomScoreAdjMin = -1000
)

var memoryLimits struct {
	mu sync.Mutex

	// limit is what SetMemoryLimit set, or nil.
	//
	// +checklocks:mu
	limit *MemoryLimit

	// limiters are the memoryLimiters of the application MemoryFiles of the
	// started Kernels.
	//
	// +checklocks:mu
	limiters map[*pgalloc.MemoryFile]*memoryLimiter
}

// SetMemoryLimit has each Kernel that starts afterwards keep its processes,
// with those of the other Kernels sharing its application MemoryFile, within
// limit, as Linux keeps a cgroup's processes within the cgroup's memory
// limit: when they would hold more than the limit allows, it kills the
// process holding the most memory, and they run no application code until
// they hold less.
//
// The host enforces the limit, and its out-of-memory killer would kill a
// process too, the stub of the one holding the most memory resident under
// systrap. But killing a stub frees nothing: the process's pages are in the
// MemoryFile until the Sentry releases them, and meanwhile the cgroup stays
// full, so that the host went on killing processes, a family's init among
// them, before the Sentry could free the first's. The Sentry allocates each
// page an application may use before the application uses it, so it can kill
// first, and the processes it kills free their memory at once.
func SetMemoryLimit(limit MemoryLimit) {
	memoryLimits.mu.Lock()
	defer memoryLimits.mu.Unlock()
	memoryLimits.limit = &limit
	if memoryLimits.limiters == nil {
		memoryLimits.limiters = make(map[*pgalloc.MemoryFile]*memoryLimiter)
	}
}

// limitMemory has k keep its processes within the limit SetMemoryLimit set,
// if any, with the other Kernels sharing its MemoryFile.
//
// Preconditions: k is starting.
func (k *Kernel) limitMemory() {
	memoryLimits.mu.Lock()
	if memoryLimits.limit == nil || k.memoryLimiter != nil {
		memoryLimits.mu.Unlock()
		return
	}
	l := memoryLimits.limiters[k.mf]
	if l == nil {
		l = &memoryLimiter{
			mf:      k.mf,
			limit:   memoryLimits.limit,
			slack:   memorySlack(memoryLimits.limit.Total, k.applicationCores),
			kernels: make(map[*Kernel]struct{}),
		}
		memoryLimits.limiters[k.mf] = l
	}
	l.add(k)
	k.memoryLimiter = l
	memoryLimits.mu.Unlock()
	// The family may allocate what its cgroup may still hold from now on,
	// rather than from the first periodic check.
	if !l.check() {
		l.outOfMemory()
	}
}

// unlimitMemory undoes limitMemory.
func (k *Kernel) unlimitMemory() {
	l := k.memoryLimiter
	if l == nil {
		return
	}
	k.memoryLimiter = nil
	memoryLimits.mu.Lock()
	defer memoryLimits.mu.Unlock()
	if l.remove(k) {
		delete(memoryLimits.limiters, l.mf)
	}
}

// memorySlack is what a family may commit beyond what a check of its cgroup
// saw before the next check: a fault's allocation, at most 2 MiB, for each
// application thread that may be running at once, and the Sentry's own
// growth. It is at most an eighth of the limit.
func memorySlack(total uint64, cores uint) uint64 {
	return min(16<<20+2<<20*uint64(cores), total/8)
}

// memoryLimiter keeps the processes of the Kernels sharing an application
// MemoryFile within the memory limit of their cgroup; see SetMemoryLimit.
type memoryLimiter struct {
	mf    *pgalloc.MemoryFile
	limit *MemoryLimit
	slack uint64

	// checkMu serializes checks, which read the cgroup's files.
	checkMu sync.Mutex

	mu sync.Mutex

	// kernels are the started Kernels sharing mf.
	//
	// +checklocks:mu
	kernels map[*Kernel]struct{}

	// stop, closed, stops the goroutine that checks the cgroup every
	// memoryCheckInterval.
	//
	// +checklocks:mu
	stop chan struct{}

	// oom, while the family is over its limit, is closed once it is within
	// it again.
	//
	// +checklocks:mu
	oom chan struct{}

	// failed is true once reading the cgroup's files failed, when the
	// limiter leaves the limit to the host.
	//
	// +checklocks:mu
	failed bool

	// gaveUp is true while the family is over its limit with no process
	// left to kill, when the limiter leaves the limit to the host until the
	// family is within it again.
	//
	// +checklocks:mu
	gaveUp bool

	// victims are the processes the limiter killed last, and reclaimed is
	// what it knew of the memory being freed when it last stopped killing;
	// see startReclaim.
	//
	// +checklocks:mu
	victims []*ThreadGroup
	// +checklocks:mu
	reclaimed memoryReclaim
}

func (l *memoryLimiter) add(k *Kernel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kernels[k] = struct{}{}
	if l.stop == nil {
		l.stop = make(chan struct{})
		go l.watch(l.stop) // S/R-SAFE: not saved; see Kernel.memoryLimiter.
	}
}

// remove removes k, and returns true if no Kernel is left.
func (l *memoryLimiter) remove(k *Kernel) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.kernels, k)
	if len(l.kernels) != 0 {
		return false
	}
	if l.stop != nil {
		close(l.stop)
		l.stop = nil
	}
	l.mf.SetCommitLimit(0)
	return true
}

// watch checks the cgroup every memoryCheckInterval until stop is closed.
func (l *memoryLimiter) watch(stop chan struct{}) {
	ticker := time.NewTicker(memoryCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if !l.check() {
				l.mu.Lock()
				gaveUp := l.gaveUp
				l.mu.Unlock()
				if !gaveUp {
					l.outOfMemory()
				}
			}
		}
	}
}

// waitForMemory blocks t, about to run application code, while its family is
// over its memory limit, after killing a process to bring it back within it.
func (k *Kernel) waitForMemory(t *Task) {
	l := k.memoryLimiter
	if l == nil {
		return
	}
	l.mu.Lock()
	oom, gaveUp := l.oom, l.gaveUp
	l.mu.Unlock()
	if oom == nil {
		if l.check() || gaveUp {
			return
		}
		oom = l.outOfMemory()
	}
	// Interrupted, as when it is killed, t goes back to the run loop.
	_ = t.Block(oom)
}

// used returns what the cgroup holds that counts against its limit: its memory
// and swap, but the file cache and slab its memory reclaim may take. It also
// returns the swap.
func (l *memoryLimiter) used() (uint64, uint64, error) {
	current, err := readCgroupUint(l.limit.Current)
	if err != nil {
		return 0, 0, fmt.Errorf("reading memory.current: %w", err)
	}
	var stat [16 << 10]byte
	n, err := l.limit.Stat.ReadAt(stat[:], 0)
	if err != nil && n == 0 {
		return 0, 0, fmt.Errorf("reading memory.stat: %w", err)
	}
	var reclaimable uint64
	for _, line := range bytes.Split(stat[:n], []byte{'\n'}) {
		key, value, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			continue
		}
		switch string(key) {
		case "active_file", "inactive_file", "slab_reclaimable":
			v, err := strconv.ParseUint(string(value), 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parsing memory.stat %s: %w", key, err)
			}
			reclaimable += v
		}
	}
	used := current - min(reclaimable, current)
	var swap uint64
	if l.limit.SwapCurrent != nil {
		swap, err = readCgroupUint(l.limit.SwapCurrent)
		if err != nil {
			return 0, 0, fmt.Errorf("reading memory.swap.current: %w", err)
		}
		used += swap
	}
	return used, swap, nil
}

// total returns the most memory and swap the cgroup can hold, holding swap
// already: its limit, or, if the host has less swap free than the cgroup may
// yet use, its memory limit, the swap it holds and the host's free swap but
// the slack. Docker's default swap limit is as much again as the memory
// limit, which a host with little swap cannot give: a family limited to 8 GiB
// on a host with 3 GiB of swap free filled its cgroup at 11 GiB, long before
// the 16 GiB the Sentry kept it within, and the host's out-of-memory killer
// killed the Sentry, every branch with it. The swap held back is for what
// the cgroup's kernel memory grows by while the family is over its limit,
// besides the slack it has for that: four processes at once filling a cgroup
// that way left the host 10 MiB of swap with a slack of 24 MiB, and the host
// kills once it has none.
func (l *memoryLimiter) total(swap uint64) (uint64, error) {
	if l.limit.HostMeminfo == nil || l.limit.SwapCurrent == nil {
		return l.limit.Total, nil
	}
	free, err := readMeminfo(l.limit.HostMeminfo, "SwapFree")
	if err != nil {
		return 0, fmt.Errorf("reading the host's free swap: %w", err)
	}
	free -= min(free, l.slack)
	if l.limit.Memory+swap+free < l.limit.Total {
		return l.limit.Memory + swap + free, nil
	}
	return l.limit.Total, nil
}

// readMeminfo returns the field of a /proc/meminfo, in bytes.
func readMeminfo(f *os.File, field string) (uint64, error) {
	var buf [8 << 10]byte
	n, err := f.ReadAt(buf[:], 0)
	if err != nil && n == 0 {
		return 0, err
	}
	for _, line := range bytes.Split(buf[:n], []byte{'\n'}) {
		key, value, ok := bytes.Cut(line, []byte{':'})
		if !ok || string(key) != field {
			continue
		}
		// As in "SwapFree:        3036312 kB".
		fields := bytes.Fields(value)
		if len(fields) != 2 || string(fields[1]) != "kB" {
			return 0, fmt.Errorf("parsing %s %q", field, value)
		}
		kib, err := strconv.ParseUint(string(fields[0]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing %s: %w", field, err)
		}
		return kib << 10, nil
	}
	return 0, fmt.Errorf("no %s", field)
}

func readCgroupUint(f *os.File) (uint64, error) {
	var buf [32]byte
	n, err := f.ReadAt(buf[:], 0)
	if err != nil && n == 0 {
		return 0, err
	}
	return strconv.ParseUint(string(bytes.TrimSpace(buf[:n])), 10, 64)
}

// check returns false if the family is over its limit. Otherwise it lets the
// family allocate as much as its cgroup may still hold before it checks again.
func (l *memoryLimiter) check() bool {
	_, _, within := l.checkUsed()
	return within
}

// checkUsed is check, and also returns what the cgroup holds that counts
// against its limit, and the limit; see total.
func (l *memoryLimiter) checkUsed() (uint64, uint64, bool) {
	l.checkMu.Lock()
	defer l.checkMu.Unlock()
	l.mu.Lock()
	failed := l.failed
	l.mu.Unlock()
	if failed {
		return 0, 0, true
	}
	used, swap, err := l.used()
	var total uint64
	if err == nil {
		total, err = l.total(swap)
	}
	if err != nil {
		log.Warningf("Leaving the memory limit of %d MiB to the host: %v", l.limit.Memory>>20, err)
		l.mu.Lock()
		l.failed = true
		l.mu.Unlock()
		l.mf.SetCommitLimit(0)
		return 0, 0, true
	}
	threshold := total - min(l.slack, total/8)
	if used >= threshold {
		return used, total, false
	}
	l.mf.SetCommitLimit(l.mf.AccountedBytes() + threshold - used)
	l.mu.Lock()
	l.gaveUp = false
	l.mu.Unlock()
	return used, total, true
}

// outOfMemory starts bringing the family back within its limit, unless it is
// doing so already, and returns a channel closed once it is.
func (l *memoryLimiter) outOfMemory() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.oom == nil {
		l.oom = make(chan struct{})
		// Every task about to run application code waits.
		l.mf.SetCommitLimit(1)
		go l.reclaim(l.oom) // S/R-SAFE: not saved; see Kernel.memoryLimiter.
	}
	return l.oom
}

// reclaim kills processes until the family is within its limit, then closes
// done. It kills one only once nothing frees memory any more, as Linux's
// out-of-memory killer waits for its victim's memory to be freed.
func (l *memoryLimiter) reclaim(done chan struct{}) {
	victims, r := l.startReclaim(time.Now())
	defer func() {
		l.mu.Lock()
		l.oom = nil
		l.victims, l.reclaimed = victims, r
		l.mu.Unlock()
		close(done)
	}()
	for {
		used, total, within := l.checkUsed()
		if within {
			return
		}
		now := time.Now()
		if r.stalled(now, used, allExited(victims), l.mf.HasWaste()) {
			killed := l.kill(total)
			if len(killed) == 0 {
				// Nothing more to kill: the host's limit decides.
				log.Warningf("Out of memory, with no process to kill: leaving the limit of %d MiB to the host", total>>20)
				l.mu.Lock()
				l.gaveUp = true
				l.mu.Unlock()
				l.mf.SetCommitLimit(0)
				return
			}
			victims = killed
			r = memoryReclaim{killed: true, killedAt: now, least: used, progressAt: now}
		}
		time.Sleep(memoryKillPollInterval)
	}
}

// startReclaim returns the processes that a reclaim starting at now waits
// for, and its state: those the limiter killed last and what it knew of the
// memory being freed, while they may still free memory. The family may have
// been within its limit for a moment since it killed them, as what its cgroup
// holds goes down and up while the host swaps the Sentry's own memory out and
// in, but their memory is still to be freed, and Linux's out-of-memory killer
// kills no other process while its victim exits.
func (l *memoryLimiter) startReclaim(now time.Time) ([]*ThreadGroup, memoryReclaim) {
	l.mu.Lock()
	victims, r := l.victims, l.reclaimed
	l.mu.Unlock()
	if len(victims) == 0 || (allExited(victims) && now.Sub(r.killedAt) >= memoryKillPatience) {
		return nil, memoryReclaim{least: math.MaxUint64}
	}
	return victims, r
}

// memoryReclaim is what reclaim knows of the memory being freed since it last
// killed.
type memoryReclaim struct {
	// killed is true once reclaim has killed, and killedAt is when it last
	// did.
	killed   bool
	killedAt time.Time

	// least is the least the family has held since reclaim last killed, and
	// progressAt is when what it held last shrank by memoryProgress.
	least      uint64
	progressAt time.Time

	// settledAt, unless zero, is when nothing freed memory any more.
	settledAt time.Time
}

// stalled returns true if reclaim should kill, with the family holding used at
// now: if nothing will bring it back within its limit. exited is true if the
// processes reclaim killed last have exited, and waste is true if the
// MemoryFile has yet to release memory freed.
func (r *memoryReclaim) stalled(now time.Time, used uint64, exited, waste bool) bool {
	if used+memoryProgress <= r.least {
		r.least, r.progressAt = used, now
	}
	if !exited || waste {
		// Memory is being freed: the killed processes' as they exit, and
		// what was freed as the MemoryFile releases it, which Linux would
		// have freed at once.
		r.settledAt = time.Time{}
		return now.Sub(r.progressAt) >= memoryKillPatience
	}
	if !r.killed {
		return true
	}
	if r.settledAt.IsZero() {
		r.settledAt = now
	}
	// Give what the cgroup holds time to show the memory freed.
	since := r.progressAt
	if r.settledAt.After(since) {
		since = r.settledAt
	}
	return now.Sub(since) >= memoryKillGrace
}

func allExited(tgs []*ThreadGroup) bool {
	for _, tg := range tgs {
		exited := false
		tg.WithTaskSetRLock(func() { exited = tg.liveTasks == 0 })
		if !exited {
			return false
		}
	}
	return true
}

// kill kills the process Linux's out-of-memory killer would, with the cgroup
// holding no more than total: the one with the most memory resident, as its
// oom_score_adj weighs it, of those it may kill, in Kernels that are not
// paused, and every other process sharing its memory. It returns them.
func (l *memoryLimiter) kill(total uint64) []*ThreadGroup {
	l.mu.Lock()
	kernels := make([]*Kernel, 0, len(l.kernels))
	for k := range l.kernels {
		kernels = append(kernels, k)
	}
	l.mu.Unlock()
	type candidate struct {
		tg  *ThreadGroup
		mm  *mm.MemoryManager
		adj int32
	}
	var candidates []candidate
	for _, k := range kernels {
		if k.IsPaused() {
			// A paused branch's processes would free nothing until it
			// resumed.
			continue
		}
		k.tasks.mu.RLock()
		for tg := range k.tasks.Root.tgids {
			leader := tg.leader
			if leader == nil || tg.liveTasks == 0 {
				continue
			}
			tg.signalHandlers.mu.Lock()
			exiting := tg.exiting
			tg.signalHandlers.mu.Unlock()
			if exiting {
				continue
			}
			leader.mu.Lock()
			m := leader.image.MemoryManager
			leader.mu.Unlock()
			if m == nil {
				continue
			}
			candidates = append(candidates, candidate{tg: tg, mm: m, adj: tg.oomScoreAdj.Load()})
		}
		k.tasks.mu.RUnlock()
	}
	var victim *candidate
	var victimRSS uint64
	victimPoints := int64(math.MinInt64)
	for i := range candidates {
		c := &candidates[i]
		if c.adj == oomScoreAdjMin {
			continue
		}
		rss := c.mm.ResidentSetSize()
		if points := int64(rss) + int64(c.adj)*int64(total/1000); points > victimPoints {
			victim, victimRSS, victimPoints = c, rss, points
		}
	}
	if victim == nil {
		return nil
	}
	var killed []*ThreadGroup
	for i := range candidates {
		if c := &candidates[i]; c.mm == victim.mm {
			c.tg.oomKilled.Store(true)
			c.tg.SendSignal(SignalInfoPriv(linux.SIGKILL))
			killed = append(killed, c.tg)
		}
	}
	log.Warningf("Out of memory: killed process %d with %d MiB resident, and %d others sharing its memory, to keep within the limit of %d MiB", victim.tg.ID(), victimRSS>>20, len(killed)-1, total>>20)
	return killed
}
