// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"strings"
	"testing"
	"time"
)

func TestSyslogRecords(t *testing.T) {
	var s syslog
	first := s.FirstSeq()
	next := s.NextSeq()
	if next <= first {
		t.Fatalf("the easter egg has no records: first %d, next %d", first, next)
	}
	s.Append(1<<3|6, time.Second, "init: hello")
	r, after, ok := s.Record(next)
	if !ok || r.Seq != next || r.Prio != 1<<3|6 || r.Msg != "init: hello" || after != next+1 {
		t.Errorf("Record(%d) = %+v, %d, %t", next, r, after, ok)
	}
	if _, after, ok := s.Record(next + 1); ok || after != next+1 {
		t.Errorf("Record past the end = %d, %t; want %d, false", after, ok, next+1)
	}
	if log := string(s.Log()); !strings.Contains(log, "<6>[    1.000000] init: hello\n") {
		t.Errorf("Log() lacks the record: %q", log)
	}

	// The log drops its oldest records past its size.
	msg := strings.Repeat("x", SyslogMaxMessage)
	for i := 0; i < syslogMaxBytes/SyslogMaxMessage+2; i++ {
		s.Append(6, 0, msg)
	}
	oldest := s.FirstSeq()
	if oldest <= next {
		t.Fatalf("the log kept its oldest records: first %d", oldest)
	}
	if _, after, ok := s.Record(first); ok || after != oldest {
		t.Errorf("Record of a dropped record = %d, %t; want %d, false", after, ok, oldest)
	}
	if s.size > syslogMaxBytes {
		t.Errorf("the log keeps %d bytes, more than %d", s.size, syslogMaxBytes)
	}

	// Messages are bounded.
	s.Append(6, 0, msg+"y")
	if r, _, _ := s.Record(s.NextSeq() - 1); len(r.Msg) != SyslogMaxMessage {
		t.Errorf("a long message is %d bytes, want %d", len(r.Msg), SyslogMaxMessage)
	}
}
