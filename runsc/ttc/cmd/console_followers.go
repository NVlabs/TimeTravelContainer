// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

// A family's console follower keeps the logs of the family's followed
// consoles: see ttcconsole.FollowerSocketName. ttcd starts one when it
// follows a console of a family that has none, and it exits a while after
// it follows none; it outlives ttcd, as its consoles do.

// followerCallTimeout bounds a request to a follower, which asks at most one
// thing of its Sentry.
const followerCallTimeout = 30 * time.Second

// errFollowerGone is the error of a request whose follower closed its
// connection first: it exited, for want of consoles or otherwise.
var errFollowerGone = errors.New("the family's console follower is gone")

// familyFollowerClient is a connection to a family's console follower.
type familyFollowerClient struct {
	conn    *net.UnixConn
	mu      sync.Mutex
	nextID  uint64
	replies map[uint64]chan ttcconsole.FollowerMessage
	gone    chan struct{}
}

// dialFamilyFollower connects to the console follower of the family whose
// console directory is dir. The follower's Done notices go to done, and lost
// runs once the connection ends.
func dialFamilyFollower(dir string, done func(lock string), lost func(*familyFollowerClient)) (*familyFollowerClient, error) {
	address, closeDir, err := ttcconsole.SocketAddress(filepath.Join(dir, ttcconsole.FollowerSocketName))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUnix(ttcconsole.FollowerNetwork, nil, &net.UnixAddr{Name: address, Net: ttcconsole.FollowerNetwork})
	closeDir()
	if err != nil {
		return nil, err
	}
	client := &familyFollowerClient{conn: conn, replies: make(map[uint64]chan ttcconsole.FollowerMessage), gone: make(chan struct{})}
	go client.read(done, lost)
	return client, nil
}

func (c *familyFollowerClient) read(done func(string), lost func(*familyFollowerClient)) {
	for {
		message, files, err := ttcconsole.ReadFollowerMessage(c.conn)
		for _, file := range files {
			_ = file.Close()
		}
		if err != nil {
			break
		}
		if message.Done != "" {
			if done != nil {
				done(message.Done)
			}
			continue
		}
		c.mu.Lock()
		reply := c.replies[message.ID]
		delete(c.replies, message.ID)
		c.mu.Unlock()
		if reply != nil {
			reply <- message
		}
	}
	_ = c.conn.Close()
	c.mu.Lock()
	c.replies = nil
	close(c.gone)
	c.mu.Unlock()
	if lost != nil {
		lost(c)
	}
}

// call sends a request and waits for its reply.
func (c *familyFollowerClient) call(message ttcconsole.FollowerMessage, files ...*os.File) error {
	reply := make(chan ttcconsole.FollowerMessage, 1)
	c.mu.Lock()
	if c.replies == nil {
		c.mu.Unlock()
		return errFollowerGone
	}
	c.nextID++
	message.ID = c.nextID
	c.replies[message.ID] = reply
	c.mu.Unlock()
	if err := ttcconsole.WriteFollowerMessage(c.conn, message, files...); err != nil {
		c.mu.Lock()
		if c.replies != nil {
			delete(c.replies, message.ID)
		}
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", errFollowerGone, err)
	}
	timer := time.NewTimer(followerCallTimeout)
	defer timer.Stop()
	select {
	case answer := <-reply:
		if answer.Error != "" {
			return errors.New(answer.Error)
		}
		return nil
	case <-c.gone:
		return errFollowerGone
	case <-timer.C:
		return fmt.Errorf("the family's console follower did not answer %q in %s", message.Op, followerCallTimeout)
	}
}

func (c *familyFollowerClient) isGone() bool {
	select {
	case <-c.gone:
		return true
	default:
		return false
	}
}

// daemonFollowedConsole is a console whose end this daemon observes, which a
// family follower follows.
type daemonFollowedConsole struct {
	dir    string
	client *familyFollowerClient
	// pending is set while the follower is asked to follow the console:
	// losing it then means only that it did not.
	pending         bool
	target          selector
	observeInitExit bool
}

// familyFollower returns a connection to the follower of the family whose
// console directory is dir, starting the follower with start if none runs.
// Connecting to a family's follower, and starting it, is one at a time.
func (s *daemonServer) familyFollower(dir string, start func() error) (*familyFollowerClient, error) {
	s.followerMu.Lock()
	if client := s.followers[dir]; client != nil && !client.isGone() {
		s.followerMu.Unlock()
		return client, nil
	}
	if s.followerStarts == nil {
		s.followerStarts = make(map[string]*sync.Mutex)
	}
	starting := s.followerStarts[dir]
	if starting == nil {
		starting = &sync.Mutex{}
		s.followerStarts[dir] = starting
	}
	s.followerMu.Unlock()
	starting.Lock()
	defer starting.Unlock()
	s.followerMu.Lock()
	if client := s.followers[dir]; client != nil && !client.isGone() {
		s.followerMu.Unlock()
		return client, nil
	}
	s.followerMu.Unlock()
	lost := func(client *familyFollowerClient) { s.familyFollowerLost(dir, client) }
	client, err := dialFamilyFollower(dir, s.familyConsoleDone, lost)
	if err != nil && start != nil {
		if err = start(); err == nil {
			client, err = dialFamilyFollower(dir, s.familyConsoleDone, lost)
		}
	}
	if err != nil {
		return nil, err
	}
	s.followerMu.Lock()
	defer s.followerMu.Unlock()
	if s.followers == nil {
		s.followers = make(map[string]*familyFollowerClient)
	}
	s.followers[dir] = client
	return client, nil
}

// errFollowerExiting is how a follower refuses a console as it exits.
const errFollowerExiting = "the console follower is exiting"

// FollowFamilyConsole has the console follower of the family whose console
// directory is dir follow a console, with its lock, and observes its end.
// start starts the follower if none runs.
func (s *daemonServer) FollowFamilyConsole(dir string, start func() error, follow ttcconsole.Follow, lock *os.File, target selector, observeInitExit bool) error {
	key := follow.Lock
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var client *familyFollowerClient
		if client, err = s.familyFollower(dir, start); err != nil {
			return err
		}
		// Observe first: the console may end as soon as it is followed.
		s.observeFamilyConsole(key, daemonFollowedConsole{dir: dir, client: client, pending: true, target: target, observeInitExit: observeInitExit})
		err = client.call(ttcconsole.FollowerMessage{Op: ttcconsole.FollowerFollow, Follow: &follow}, lock)
		s.followerMu.Lock()
		console, observed := s.familyConsoles[key]
		if err == nil {
			lostMeanwhile := false
			if observed && console.client == client {
				console.pending = false
				s.familyConsoles[key] = console
				lostMeanwhile = client.isGone()
			}
			s.followerMu.Unlock()
			if lostMeanwhile {
				s.familyConsoleDone(key)
			}
			return nil
		}
		if observed && console.client == client {
			delete(s.familyConsoles, key)
		}
		if s.followers[dir] == client && client.isGone() {
			delete(s.followers, dir)
		}
		s.followerMu.Unlock()
		// A follower that was exiting for want of consoles took no request:
		// start another.
		if !errors.Is(err, errFollowerGone) && err.Error() != errFollowerExiting {
			break
		}
		<-client.gone
	}
	return err
}

// ObserveFamilyConsole observes the end of a console that the follower of the
// family whose console directory is dir may follow, one started by an older
// ttcd. It reports false if no follower runs there.
func (s *daemonServer) ObserveFamilyConsole(dir, key string, target selector, observeInitExit bool) bool {
	client, err := s.familyFollower(dir, nil)
	if err != nil {
		return false
	}
	s.observeFamilyConsole(key, daemonFollowedConsole{dir: dir, client: client, target: target, observeInitExit: observeInitExit})
	if err := client.call(ttcconsole.FollowerMessage{Op: ttcconsole.FollowerObserve, Lock: key}); err != nil && !errors.Is(err, errFollowerGone) {
		s.followerMu.Lock()
		delete(s.familyConsoles, key)
		s.followerMu.Unlock()
		return false
	}
	return true
}

// StopFamilyConsole stops following the console whose lock is key, or all of
// the family's consoles if key is empty, if the family whose console
// directory is dir has a follower. It reports whether it has.
func (s *daemonServer) StopFamilyConsole(dir, key string) (bool, error) {
	client, err := s.familyFollower(dir, nil)
	if err != nil {
		return false, nil
	}
	if err := client.call(ttcconsole.FollowerMessage{Op: ttcconsole.FollowerStop, Lock: key}); err != nil && !errors.Is(err, errFollowerGone) {
		return true, err
	}
	return true, nil
}

func (s *daemonServer) observeFamilyConsole(key string, console daemonFollowedConsole) {
	s.followerMu.Lock()
	defer s.followerMu.Unlock()
	if s.familyConsoles == nil {
		s.familyConsoles = make(map[string]daemonFollowedConsole)
	}
	s.familyConsoles[key] = console
}

// familyConsoleDone runs when a follower releases a console: its log and
// status are complete.
func (s *daemonServer) familyConsoleDone(key string) {
	s.followerMu.Lock()
	console, observed := s.familyConsoles[key]
	delete(s.familyConsoles, key)
	s.followerMu.Unlock()
	if !observed || !console.observeInitExit {
		return
	}
	// Finishing the exit may ask the follower about other consoles, which
	// needs this connection's reader.
	go func() {
		if !s.consumeSuppressedConsoleExit(key) {
			s.observeInitConsoleExit(console.target)
		}
	}()
}

// familyFollowerLost runs when the connection to a follower ends. A follower
// that is gone has released every console it followed.
func (s *daemonServer) familyFollowerLost(dir string, client *familyFollowerClient) {
	s.followerMu.Lock()
	if s.followers[dir] == client {
		delete(s.followers, dir)
		if starting := s.followerStarts[dir]; starting != nil && starting.TryLock() {
			delete(s.followerStarts, dir)
			starting.Unlock()
		}
	}
	var keys []string
	for key, console := range s.familyConsoles {
		if console.client == client && !console.pending {
			keys = append(keys, key)
		}
	}
	s.followerMu.Unlock()
	for _, key := range keys {
		s.familyConsoleDone(key)
	}
}

// familyConsoleObserved reports whether this daemon observes the end of the
// console whose lock is key.
func (s *daemonServer) familyConsoleObserved(key string) bool {
	s.followerMu.Lock()
	defer s.followerMu.Unlock()
	_, observed := s.familyConsoles[key]
	return observed
}
