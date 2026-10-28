// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"bytes"
	"fmt"
	"math/rand"
	"time"

	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

// SyslogRecord is a record of the kernel log.
//
// +stateify savable
type SyslogRecord struct {
	// Seq is the record's sequence number.
	Seq uint64
	// Prio is the record's facility and level, facility<<3 | level.
	Prio uint32
	// Time is when the record was logged, since boot.
	Time time.Duration
	// Msg is the record's message.
	Msg string
}

const (
	// SyslogMaxBytes bounds the messages that the kernel log keeps: it drops
	// its oldest records to take new ones. It is the log's size, as
	// syslog(2)'s SYSLOG_ACTION_SIZE_BUFFER reports it.
	SyslogMaxBytes = 1 << 20
	// SyslogMaxMessage bounds a record's message.
	SyslogMaxMessage = 8 << 10
)

// syslog represents a sentry-global kernel log: the records that /dev/kmsg
// writes, and syslog(2) and /dev/kmsg read. It starts with fun messages for
// a dmesg easter egg.
//
// +stateify savable
type syslog struct {
	mu sync.Mutex `state:"nosave"`

	// initialized is whether records has the easter egg's messages.
	initialized bool

	// records are the log's records, oldest first, with consecutive
	// sequence numbers.
	records []SyslogRecord

	// size is the total length of the records' messages.
	size int

	// nextSeq is the sequence number of the next record.
	nextSeq uint64

	// queue is notified of new records.
	queue waiter.Queue
}

// initLocked adds the easter egg's messages to the log once.
//
// Preconditions: s.mu is locked.
func (s *syslog) initLocked() {
	if s.initialized {
		return
	}
	s.initialized = true

	allMessages := []string{
		"Synthesizing system calls...",
		"Mounting deweydecimalfs...",
		"Moving files to filing cabinet...",
		"Digging up root...",
		"Constructing home...",
		"Segmenting fault lines...",
		"Creating bureaucratic processes...",
		"Searching for needles in stacks...",
		"Preparing for the zombie uprising...",
		"Feeding the init monster...",
		"Creating cloned children...",
		"Daemonizing children...",
		"Waiting for children...",
		"Gathering forks...",
		"Committing treasure map to memory...",
		"Reading process obituaries...",
		"Searching for socket adapter...",
		"Creating process schedule...",
		"Generating random numbers by fair dice roll...",
		"Rewriting operating system in Javascript...",
		"Reticulating splines...",
		"Consulting tar man page...",
		"Forking spaghetti code...",
		"Checking naughty and nice process list...",
		"Checking naughty and nice process list...", // Check it up to twice.
		"Granting licence to kill(2)...",            // British spelling for British movie.
		"Letting the watchdogs out...",
		"Conjuring /dev/null black hole...",
		"Adversarially training Redcode AI...",
		"Singleplexing /dev/ptmx...",
		"Recruiting cron-ies...",
		"Verifying that no non-zero bytes made their way into /dev/zero...",
		"Accelerating teletypewriter to 9600 baud...",
		"DeFUSEing fork bombs...",
		"Deleting VFS and rebuilding it from scratch...",
		"Asking an AI agent to fix the bugs...",
		"Rewriting the kernel in Rust...",
		"Politicking the oom killer...",
	}

	selectMessage := func() string {
		i := rand.Intn(len(allMessages))
		m := allMessages[i]

		// Delete the selected message.
		allMessages[i] = allMessages[len(allMessages)-1]
		allMessages = allMessages[:len(allMessages)-1]

		return m
	}

	// The messages are the kernel's, at level 6, KERN_INFO.
	const prio = 6
	var eggs []SyslogRecord
	eggs = append(eggs, SyslogRecord{Prio: prio, Msg: "Starting gVisor..."})
	t := 0.1
	for i := 0; i < 10; i++ {
		t += rand.Float64() / 2
		eggs = append(eggs, SyslogRecord{Prio: prio, Time: time.Duration(t * float64(time.Second)), Msg: selectMessage()})
	}
	t += rand.Float64() / 2
	eggs = append(eggs, SyslogRecord{Prio: prio, Time: time.Duration(t * float64(time.Second)), Msg: "Ready!"})

	// Records logged before the easter egg follow it.
	records := s.records
	s.records = nil
	s.size = 0
	s.nextSeq = 0
	for _, r := range eggs {
		s.appendLocked(r.Prio, r.Time, r.Msg)
	}
	for _, r := range records {
		s.appendLocked(r.Prio, r.Time, r.Msg)
	}
}

// appendLocked adds a record.
//
// Preconditions: s.mu is locked.
func (s *syslog) appendLocked(prio uint32, t time.Duration, msg string) {
	if len(msg) > SyslogMaxMessage {
		msg = msg[:SyslogMaxMessage]
	}
	s.records = append(s.records, SyslogRecord{Seq: s.nextSeq, Prio: prio, Time: t, Msg: msg})
	s.nextSeq++
	s.size += len(msg)
	drop := 0
	for s.size > SyslogMaxBytes && drop < len(s.records)-1 {
		s.size -= len(s.records[drop].Msg)
		drop++
	}
	if drop != 0 {
		s.records = append(s.records[:0], s.records[drop:]...)
	}
}

// Append adds a record, of message msg, with facility and level prio,
// logged t after boot.
func (s *syslog) Append(prio uint32, t time.Duration, msg string) {
	s.mu.Lock()
	s.initLocked()
	s.appendLocked(prio, t, msg)
	s.mu.Unlock()
	s.queue.Notify(waiter.ReadableEvents)
}

// Log returns the log, as syslog(2) reads it.
func (s *syslog) Log() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	var buf bytes.Buffer
	for _, r := range s.records {
		fmt.Fprintf(&buf, "<%d>[%12.6f] %s\n", r.Prio&7, r.Time.Seconds(), r.Msg)
	}
	return buf.Bytes()
}

// FirstSeq returns the sequence number of the log's oldest record.
func (s *syslog) FirstSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	return s.records[0].Seq
}

// NextSeq returns the sequence number of the log's next record.
func (s *syslog) NextSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	return s.nextSeq
}

// Record returns the record of sequence number seq. If the log has dropped
// it, it returns ok false and the oldest record's sequence number; if the
// log has no such record yet, it returns ok false and seq.
func (s *syslog) Record(seq uint64) (r SyslogRecord, next uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	first := s.records[0].Seq
	if seq < first {
		return SyslogRecord{}, first, false
	}
	if seq >= s.nextSeq {
		return SyslogRecord{}, seq, false
	}
	return s.records[seq-first], seq + 1, true
}

// EventRegister registers e for new records.
func (s *syslog) EventRegister(e *waiter.Entry) {
	s.queue.EventRegister(e)
}

// EventUnregister unregisters e.
func (s *syslog) EventUnregister(e *waiter.Entry) {
	s.queue.EventUnregister(e)
}
