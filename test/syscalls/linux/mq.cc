// Copyright 2021 The gVisor Authors.
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

/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include <fcntl.h>
#include <linux/capability.h>
#include <mqueue.h>
#include <sched.h>
#include <signal.h>
#include <sys/poll.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#include <cerrno>
#include <cstddef>
#include <cstdint>
#include <initializer_list>
#include <string>
#include <utility>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/flags/flag.h"
#include "absl/strings/str_format.h"
#include "absl/time/clock.h"
#include "absl/time/time.h"
#include "test/util/capability_util.h"
#include "test/util/cleanup.h"
#include "test/util/file_descriptor.h"
#include "test/util/fs_util.h"
#include "test/util/linux_capability_util.h"
#include "test/util/mount_util.h"
#include "test/util/posix_error.h"
#include "test/util/signal_util.h"
#include "test/util/temp_path.h"
#include "test/util/test_util.h"
#include "test/util/thread_util.h"

#define NAME_MAX 255

ABSL_FLAG(int32_t, scratch_uid, 65534, "scratch UID");

namespace gvisor {
namespace testing {
namespace {

// PosixQueue is a RAII class used to automatically clean POSIX message queues.
class PosixQueue {
 public:
  PosixQueue(mqd_t fd, std::string name) : fd_(fd), name_(std::string(name)) {}
  PosixQueue(const PosixQueue&) = delete;
  PosixQueue& operator=(const PosixQueue&) = delete;

  // Move constructor.
  PosixQueue(PosixQueue&& q) {
    fd_ = q.fd_;
    name_ = q.name_;
    // Call PosixQueue::release, to prevent the object being released from
    // unlinking the underlying queue.
    q.release();
  }

  ~PosixQueue() {
    if (fd_ != -1) {
      EXPECT_THAT(mq_close(fd_), SyscallSucceeds());
      EXPECT_THAT(mq_unlink(name_.c_str()), SyscallSucceeds());
    }
  }

  mqd_t fd() { return fd_; }

  const char* name() { return name_.c_str(); }

  mqd_t release() {
    mqd_t old = fd_;
    fd_ = -1;
    return old;
  }

 private:
  mqd_t fd_;
  std::string name_;
};

// MqOpen wraps mq_open(3) using a given name.
PosixErrorOr<PosixQueue> MqOpen(std::string name, int oflag) {
  mqd_t fd = mq_open(name.c_str(), oflag);
  if (fd == -1) {
    return PosixError(errno, absl::StrFormat("mq_open(%s, %d)", name, oflag));
  }
  return PosixQueue(fd, name);
}

// MqOpen wraps mq_open(3) using a given name.
PosixErrorOr<PosixQueue> MqOpen(int oflag, mode_t mode, struct mq_attr* attr) {
  auto name = "/" + NextTempBasename();
  mqd_t fd = mq_open(name.c_str(), oflag, mode, attr);
  if (fd == -1) {
    return PosixError(errno, absl::StrFormat("mq_open(%d)", oflag));
  }
  return PosixQueue(fd, name);
}

// MqOpen wraps mq_open(3) using a generated name.
PosixErrorOr<PosixQueue> MqOpen(std::string name, int oflag, mode_t mode,
                                struct mq_attr* attr) {
  mqd_t fd = mq_open(name.c_str(), oflag, mode, attr);
  if (fd == -1) {
    return PosixError(errno, absl::StrFormat("mq_open(%d)", oflag));
  }
  return PosixQueue(fd, name);
}

// MqUnlink wraps mq_unlink(2).
PosixError MqUnlink(std::string name) {
  int err = mq_unlink(name.c_str());
  if (err == -1) {
    return PosixError(errno, absl::StrFormat("mq_unlink(%s)", name.c_str()));
  }
  return NoError();
}

// MqClose wraps mq_close(2).
PosixError MqClose(mqd_t fd) {
  int err = mq_close(fd);
  if (err == -1) {
    return PosixError(errno, absl::StrFormat("mq_close(%d)", fd));
  }
  return NoError();
}

// Test simple opening and closing of a message queue.
TEST(MqTest, Open) {
  ASSERT_NO_ERRNO(MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));
}

TEST(MqTest, ModeWithFileType) {
  // S_IFIFO should be ignored.
  ASSERT_NO_ERRNO(MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777 | S_IFIFO, nullptr));
}

// Test mq_open(2) after mq_unlink(2).
TEST(MqTest, OpenAfterUnlink) {
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  ASSERT_NO_ERRNO(MqUnlink(queue.name()));
  EXPECT_THAT(MqOpen(queue.name(), O_RDWR), PosixErrorIs(ENOENT));
  ASSERT_NO_ERRNO(MqClose(queue.release()));
}

// Test using invalid args with mq_open.
TEST(MqTest, OpenInvalidArgs) {
  // Name must start with a slash.
  EXPECT_THAT(MqOpen("test", O_RDWR), PosixErrorIs(EINVAL));

  // Name can't contain more that one slash.
  EXPECT_THAT(MqOpen("/test/name", O_RDWR), PosixErrorIs(EACCES));

  // Both "." and ".." can't be used as queue names.
  EXPECT_THAT(MqOpen(".", O_RDWR), PosixErrorIs(EINVAL));
  EXPECT_THAT(MqOpen("..", O_RDWR), PosixErrorIs(EINVAL));

  // mq_attr's mq_maxmsg and mq_msgsize must be > 0.
  struct mq_attr attr;
  attr.mq_maxmsg = -1;
  attr.mq_msgsize = 10;

  EXPECT_THAT(MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr),
              PosixErrorIs(EINVAL));

  attr.mq_maxmsg = 10;
  attr.mq_msgsize = -1;

  EXPECT_THAT(MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr),
              PosixErrorIs(EINVAL));

  // Names should be shorter than NAME_MAX.
  char max[NAME_MAX + 3];
  max[0] = '/';
  for (size_t i = 1; i < NAME_MAX + 2; i++) {
    max[i] = 'a';
  }
  max[NAME_MAX + 2] = '\0';

  EXPECT_THAT(MqOpen(std::string(max), O_RDWR | O_CREAT | O_EXCL, 0777, &attr),
              PosixErrorIs(ENAMETOOLONG));
}

// Test creating a queue that already exists.
TEST(MqTest, CreateAlreadyExists) {
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  EXPECT_THAT(MqOpen(queue.name(), O_RDWR | O_CREAT | O_EXCL, 0777, nullptr),
              PosixErrorIs(EEXIST));
}

// Test opening a queue that doesn't exists.
TEST(MqTest, NoQueueExists) {
  // Choose a name to pass that's unlikely to exist if the test is run locally.
  EXPECT_THAT(MqOpen("/gvisor-mq-test-nonexistent-queue", O_RDWR),
              PosixErrorIs(ENOENT));
}

TEST(MqTest, UnlinkOtherUserQueue) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SETUID)));
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0600, nullptr));

  // Change only this thread's credentials so the owner can clean up afterward.
  ScopedThread([&] {
    AutoCapability fowner(CAP_FOWNER, false);
    ASSERT_THAT(
        syscall(SYS_setresuid, -1, absl::GetFlag(FLAGS_scratch_uid), -1),
        SyscallSucceeds());
    EXPECT_THAT(MqUnlink(queue.name()), PosixErrorIs(EACCES));
  });
}

// Test trying to re-open a queue with invalid permissions.
TEST(MqTest, OpenNoAccess) {
  AutoCapability dacOverride(CAP_DAC_OVERRIDE, false);
  AutoCapability dacReadSearch(CAP_DAC_READ_SEARCH, false);
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0000, nullptr));

  EXPECT_THAT(MqOpen(queue.name(), O_RDONLY), PosixErrorIs(EACCES));
  EXPECT_THAT(MqOpen(queue.name(), O_WRONLY), PosixErrorIs(EACCES));
  EXPECT_THAT(MqOpen(queue.name(), O_RDWR), PosixErrorIs(EACCES));
}

TEST(MqTest, OpenWithDacOverride) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_DAC_OVERRIDE)));
  AutoCapability dacReadSearch(CAP_DAC_READ_SEARCH, false);
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0000, nullptr));

  for (int mode : {O_RDONLY, O_WRONLY, O_RDWR}) {
    FileDescriptor fd(mq_open(queue.name(), mode));
    ASSERT_THAT(fd.get(), SyscallSucceeds());
  }
}

TEST(MqTest, OpenWithDacReadSearch) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_DAC_READ_SEARCH)));
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0000, nullptr));
  AutoCapability dacOverride(CAP_DAC_OVERRIDE, false);

  FileDescriptor fd(mq_open(queue.name(), O_RDONLY));
  ASSERT_THAT(fd.get(), SyscallSucceeds());
  for (int mode : {O_WRONLY, O_RDWR}) {
    FileDescriptor write_fd(mq_open(queue.name(), mode));
    EXPECT_THAT(write_fd.get(), SyscallFailsWithErrno(EACCES));
  }
}

// Test trying to re-open a read-only queue for write.
TEST(MqTest, OpenReadAccess) {
  AutoCapability dacOverride(CAP_DAC_OVERRIDE, false);
  AutoCapability dacReadSearch(CAP_DAC_READ_SEARCH, false);
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0400, nullptr));

  EXPECT_THAT(MqOpen(queue.name(), O_WRONLY), PosixErrorIs(EACCES));
  EXPECT_NO_ERRNO(MqOpen(queue.name(), O_RDONLY));
  queue.release();
}

// Test trying to re-open a write-only queue for read.
TEST(MqTest, OpenWriteAccess) {
  AutoCapability dacOverride(CAP_DAC_OVERRIDE, false);
  AutoCapability dacReadSearch(CAP_DAC_READ_SEARCH, false);
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0200, nullptr));

  EXPECT_THAT(MqOpen(queue.name(), O_RDONLY), PosixErrorIs(EACCES));
  EXPECT_NO_ERRNO(MqOpen(queue.name(), O_WRONLY));
  queue.release();
}

// Test changing IPC namespace.
TEST(MqTest, ChangeIpcNamespace) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));

  // When changing IPC namespaces, Linux doesn't invalidate or close the
  // previously opened file descriptions and allows operations to be performed
  // on them normally, until they're closed.
  //
  // To test this we create a new queue, use unshare(CLONE_NEWIPC) to change
  // into a new IPC namespace, and trying performing a read(2) on the queue.
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  // As mq_unlink(2) uses queue's name, it should fail after changing IPC
  // namespace. To clean the queue, we should unlink it now, this should not
  // cause a problem, as the queue persists until the last mq_close(2).
  ASSERT_NO_ERRNO(MqUnlink(queue.name()));

  ASSERT_THAT(unshare(CLONE_NEWIPC), SyscallSucceeds());

  const size_t msgSize = 60;
  char queueRead[msgSize];
  ASSERT_THAT(read(queue.fd(), &queueRead[0], msgSize - 1), SyscallSucceeds());

  ASSERT_NO_ERRNO(MqClose(queue.release()));

  // Unlinking should fail now after changing IPC namespace.
  EXPECT_THAT(MqUnlink(queue.name()), PosixErrorIs(ENOENT));
}

// Test mounting the mqueue filesystem.
TEST(MqTest, Mount) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));

  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  auto const dir = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  ASSERT_NO_ERRNO(Mount("none", dir.path(), "mqueue", 0, "", 0));
}

// Test mounting the mqueue filesystem to several places.
TEST(MqTest, MountSeveral) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  constexpr int numMounts = 3;

  // mountDirs should outlive mountCUs and queue so that its destructor succeeds
  // in unlinking the mountpoints and does not interfere with queue destruction.
  testing::TempPath mountDirs[numMounts];
  testing::Cleanup mountCUs[numMounts];
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  for (int i = 0; i < numMounts; ++i) {
    mountDirs[i] = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
    mountCUs[i] = ASSERT_NO_ERRNO_AND_VALUE(
        Mount("none", mountDirs[i].path(), "mqueue", 0, "", 0));
  }

  // Ensure that queue is visible from all mounts.
  for (int i = 0; i < numMounts; ++i) {
    ASSERT_NO_ERRNO(Stat(JoinPath(mountDirs[i].path(), queue.name())));
  }
}

// Test mounting mqueue and opening a queue as normal file.
TEST(MqTest, OpenAsFile) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));

  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  auto const dir = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mnt =
      ASSERT_NO_ERRNO_AND_VALUE(Mount("none", dir.path(), "mqueue", 0, "", 0));

  // Open queue using open(2).
  auto fd = ASSERT_NO_ERRNO_AND_VALUE(
      Open(JoinPath(dir.path(), queue.name()), O_RDONLY));

  const size_t msgSize = 60;
  char queueRead[msgSize];
  queueRead[msgSize - 1] = '\0';

  ASSERT_THAT(read(fd.get(), &queueRead[0], msgSize - 1), SyscallSucceeds());

  std::string want(
      "QSIZE:0          NOTIFY:0     SIGNO:0     NOTIFY_PID:0     ");
  std::string got(queueRead);
  EXPECT_EQ(got, want);
}

// Test removing a queue using unlink(2).
TEST(MqTest, UnlinkAsFile) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));

  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  auto const dir = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mnt =
      ASSERT_NO_ERRNO_AND_VALUE(Mount("none", dir.path(), "mqueue", 0, "", 0));

  ASSERT_NO_ERRNO(
      UnlinkAt(FileDescriptor(), JoinPath(dir.path(), queue.name()), 0));

  // Trying to unlink again should fail.
  EXPECT_THAT(MqUnlink(queue.name()), PosixErrorIs(ENOENT));
  queue.release();
}

// Test read(2) from an empty queue.
TEST(MqTest, ReadEmpty) {
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  const size_t msgSize = 60;
  char queueRead[msgSize];
  queueRead[msgSize - 1] = '\0';

  ASSERT_THAT(read(queue.fd(), &queueRead[0], msgSize - 1), SyscallSucceeds());

  std::string want(
      "QSIZE:0          NOTIFY:0     SIGNO:0     NOTIFY_PID:0     ");
  std::string got(queueRead);
  EXPECT_EQ(got, want);
}

// Test poll(2) on an empty queue.
TEST(MqTest, PollEmpty) {
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, nullptr));

  struct pollfd pfd;
  pfd.fd = queue.fd();
  pfd.events = POLLOUT | POLLIN | POLLRDNORM | POLLWRNORM;

  ASSERT_THAT(poll(&pfd, 1, -1), SyscallSucceeds());
  ASSERT_EQ(pfd.revents, POLLOUT | POLLWRNORM);
}


// mq_attr for a small queue of 4 messages of at most 16 bytes.
struct mq_attr SmallQueue() {
  struct mq_attr attr = {};
  attr.mq_maxmsg = 4;
  attr.mq_msgsize = 16;
  return attr;
}

// An absolute CLOCK_REALTIME time ms milliseconds from now.
struct timespec RealtimeIn(int ms) {
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  ts.tv_nsec += static_cast<long>(ms) * 1000000;
  ts.tv_sec += ts.tv_nsec / 1000000000;
  ts.tv_nsec %= 1000000000;
  return ts;
}

// Messages come out by priority, and by arrival within one.
TEST(MqTest, SendReceivePriorityOrder) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  ASSERT_THAT(mq_send(queue.fd(), "a", 1, 1), SyscallSucceeds());
  ASSERT_THAT(mq_send(queue.fd(), "bb", 2, 5), SyscallSucceeds());
  ASSERT_THAT(mq_send(queue.fd(), "c", 1, 1), SyscallSucceeds());
  ASSERT_THAT(mq_send(queue.fd(), "dd", 2, 5), SyscallSucceeds());

  struct mq_attr got = {};
  ASSERT_THAT(mq_getattr(queue.fd(), &got), SyscallSucceeds());
  EXPECT_EQ(got.mq_curmsgs, 4);

  for (const auto& [text, priority] :
       std::vector<std::pair<std::string, unsigned>>{
           {"bb", 5}, {"dd", 5}, {"a", 1}, {"c", 1}}) {
    char buf[16];
    unsigned prio = 0;
    ASSERT_THAT(mq_receive(queue.fd(), buf, sizeof(buf), &prio),
                SyscallSucceedsWithValue(text.size()));
    EXPECT_EQ(std::string(buf, text.size()), text);
    EXPECT_EQ(prio, priority);
  }
}

TEST(MqTest, NonBlockingEmptyAndFull) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL | O_NONBLOCK, 0777, &attr));
  char buf[16];
  EXPECT_THAT(mq_receive(queue.fd(), buf, sizeof(buf), nullptr),
              SyscallFailsWithErrno(EAGAIN));
  for (int i = 0; i < attr.mq_maxmsg; i++) {
    ASSERT_THAT(mq_send(queue.fd(), "x", 1, 0), SyscallSucceeds());
  }
  EXPECT_THAT(mq_send(queue.fd(), "x", 1, 0), SyscallFailsWithErrno(EAGAIN));
}

TEST(MqTest, InvalidArguments) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  char buf[32] = {};
  EXPECT_THAT(mq_send(queue.fd(), buf, attr.mq_msgsize + 1, 0),
              SyscallFailsWithErrno(EMSGSIZE));
  EXPECT_THAT(mq_send(queue.fd(), buf, 1, MQ_PRIO_MAX),
              SyscallFailsWithErrno(EINVAL));
  ASSERT_THAT(mq_send(queue.fd(), buf, 1, 0), SyscallSucceeds());
  EXPECT_THAT(mq_receive(queue.fd(), buf, attr.mq_msgsize - 1, nullptr),
              SyscallFailsWithErrno(EMSGSIZE));
  // An invalid timeout fails even where the call need not wait.
  struct timespec invalid = {0, 1000000000};
  EXPECT_THAT(mq_timedreceive(queue.fd(), buf, sizeof(buf), nullptr, &invalid),
              SyscallFailsWithErrno(EINVAL));
  struct mq_attr got = {};
  ASSERT_THAT(mq_getattr(queue.fd(), &got), SyscallSucceeds());
  EXPECT_EQ(got.mq_curmsgs, 1);
  EXPECT_THAT(mq_send(STDIN_FILENO, buf, 1, 0), SyscallFailsWithErrno(EBADF));
}

TEST(MqTest, AccessModes) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_WRONLY | O_CREAT | O_EXCL, 0777, &attr));
  char buf[16];
  EXPECT_THAT(mq_receive(queue.fd(), buf, sizeof(buf), nullptr),
              SyscallFailsWithErrno(EBADF));
  mqd_t reader = mq_open(queue.name(), O_RDONLY);
  ASSERT_THAT(reader, SyscallSucceeds());
  EXPECT_THAT(mq_send(reader, "x", 1, 0), SyscallFailsWithErrno(EBADF));
  ASSERT_THAT(mq_send(queue.fd(), "x", 1, 0), SyscallSucceeds());
  EXPECT_THAT(mq_receive(reader, buf, sizeof(buf), nullptr),
              SyscallSucceedsWithValue(1));
  ASSERT_THAT(mq_close(reader), SyscallSucceeds());
}

TEST(MqTest, TimedReceiveTimesOut) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  char buf[16];
  struct timespec start;
  clock_gettime(CLOCK_MONOTONIC, &start);
  struct timespec deadline = RealtimeIn(100);
  EXPECT_THAT(mq_timedreceive(queue.fd(), buf, sizeof(buf), nullptr, &deadline),
              SyscallFailsWithErrno(ETIMEDOUT));
  struct timespec end;
  clock_gettime(CLOCK_MONOTONIC, &end);
  EXPECT_GE((end.tv_sec - start.tv_sec) * 1000 +
                (end.tv_nsec - start.tv_nsec) / 1000000,
            90);

  for (int i = 0; i < attr.mq_maxmsg; i++) {
    ASSERT_THAT(mq_send(queue.fd(), "x", 1, 0), SyscallSucceeds());
  }
  deadline = RealtimeIn(50);
  EXPECT_THAT(mq_timedsend(queue.fd(), "x", 1, 0, &deadline),
              SyscallFailsWithErrno(ETIMEDOUT));
}

TEST(MqTest, BlockedReceiverGetsMessage) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  mqd_t fd = queue.fd();
  ScopedThread receiver([fd] {
    char buf[16];
    unsigned prio = 0;
    TEST_PCHECK(mq_receive(fd, buf, sizeof(buf), &prio) == 3);
    TEST_CHECK(std::string(buf, 3) == "hey");
    TEST_CHECK(prio == 7);
  });
  absl::SleepFor(absl::Milliseconds(100));
  ASSERT_THAT(mq_send(fd, "hey", 3, 7), SyscallSucceeds());
  receiver.Join();
}

TEST(MqTest, GetSetAttr) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  struct mq_attr got = {};
  ASSERT_THAT(mq_getattr(queue.fd(), &got), SyscallSucceeds());
  EXPECT_EQ(got.mq_flags, 0);
  EXPECT_EQ(got.mq_maxmsg, attr.mq_maxmsg);
  EXPECT_EQ(got.mq_msgsize, attr.mq_msgsize);
  EXPECT_EQ(got.mq_curmsgs, 0);

  struct mq_attr set = {};
  set.mq_flags = O_NONBLOCK;
  struct mq_attr old = {};
  ASSERT_THAT(mq_setattr(queue.fd(), &set, &old), SyscallSucceeds());
  EXPECT_EQ(old.mq_flags, 0);
  ASSERT_THAT(mq_getattr(queue.fd(), &got), SyscallSucceeds());
  EXPECT_EQ(got.mq_flags, O_NONBLOCK);
  char buf[16];
  EXPECT_THAT(mq_receive(queue.fd(), buf, sizeof(buf), nullptr),
              SyscallFailsWithErrno(EAGAIN));

  set.mq_flags = O_NONBLOCK | O_APPEND;
  EXPECT_THAT(mq_setattr(queue.fd(), &set, nullptr),
              SyscallFailsWithErrno(EINVAL));
}

// A message into an empty queue signals the registered process once.
TEST(MqTest, NotifySignal) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  sigset_t set;
  sigemptyset(&set);
  sigaddset(&set, SIGUSR1);
  auto cleanup = ASSERT_NO_ERRNO_AND_VALUE(ScopedSignalMask(SIG_BLOCK, SIGUSR1));

  struct sigevent sev = {};
  sev.sigev_notify = SIGEV_SIGNAL;
  sev.sigev_signo = SIGUSR1;
  sev.sigev_value.sival_int = 42;
  ASSERT_THAT(mq_notify(queue.fd(), &sev), SyscallSucceeds());
  EXPECT_THAT(mq_notify(queue.fd(), &sev), SyscallFailsWithErrno(EBUSY));

  char status[64] = {};
  ASSERT_THAT(pread(queue.fd(), status, sizeof(status) - 1, 0),
              SyscallSucceeds());
  EXPECT_EQ(std::string(status).substr(0, 37),
            absl::StrFormat("QSIZE:0          NOTIFY:%-5d SIGNO:%-5d", SIGEV_SIGNAL,
                            SIGUSR1)
                .substr(0, 37));

  ASSERT_THAT(mq_send(queue.fd(), "x", 1, 0), SyscallSucceeds());
  siginfo_t info = {};
  struct timespec timeout = {5, 0};
  ASSERT_THAT(RetryEINTR(sigtimedwait)(&set, &info, &timeout),
              SyscallSucceedsWithValue(SIGUSR1));
  EXPECT_EQ(info.si_code, SI_MESGQ);
  EXPECT_EQ(info.si_value.sival_int, 42);
  EXPECT_EQ(info.si_pid, getpid());

  // The notification ended the registration: another queue's message
  // signals nothing more, and a new registration succeeds.
  ASSERT_THAT(mq_send(queue.fd(), "x", 1, 0), SyscallSucceeds());
  timeout = {0, 0};
  EXPECT_THAT(sigtimedwait(&set, &info, &timeout),
              SyscallFailsWithErrno(EAGAIN));
  ASSERT_THAT(mq_notify(queue.fd(), &sev), SyscallSucceeds());
  ASSERT_THAT(mq_notify(queue.fd(), nullptr), SyscallSucceeds());
  ASSERT_THAT(mq_notify(queue.fd(), &sev), SyscallSucceeds());
}

// A message for a receiver waiting in mq_receive notifies no one.
TEST(MqTest, NotifyNotForWaitingReceiver) {
  struct mq_attr attr = SmallQueue();
  PosixQueue queue = ASSERT_NO_ERRNO_AND_VALUE(
      MqOpen(O_RDWR | O_CREAT | O_EXCL, 0777, &attr));
  sigset_t set;
  sigemptyset(&set);
  sigaddset(&set, SIGUSR2);
  auto cleanup = ASSERT_NO_ERRNO_AND_VALUE(ScopedSignalMask(SIG_BLOCK, SIGUSR2));
  struct sigevent sev = {};
  sev.sigev_notify = SIGEV_SIGNAL;
  sev.sigev_signo = SIGUSR2;
  ASSERT_THAT(mq_notify(queue.fd(), &sev), SyscallSucceeds());

  mqd_t fd = queue.fd();
  ScopedThread receiver([fd] {
    char buf[16];
    TEST_PCHECK(mq_receive(fd, buf, sizeof(buf), nullptr) == 1);
  });
  absl::SleepFor(absl::Milliseconds(100));
  ASSERT_THAT(mq_send(fd, "x", 1, 0), SyscallSucceeds());
  receiver.Join();
  siginfo_t info;
  struct timespec timeout = {0, 0};
  EXPECT_THAT(sigtimedwait(&set, &info, &timeout),
              SyscallFailsWithErrno(EAGAIN));
  // The registration remains.
  EXPECT_THAT(mq_notify(fd, &sev), SyscallFailsWithErrno(EBUSY));
}

}  // namespace
}  // namespace testing
}  // namespace gvisor
