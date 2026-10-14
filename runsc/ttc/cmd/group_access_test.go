// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

func TestConsumeDaemonAccessOption(t *testing.T) {
	for _, test := range []struct {
		args     []string
		consumed int
		group    string
		err      bool
	}{
		{args: []string{"--unsafe-access-host-group", "proj", "--node", "n1"}, consumed: 2, group: "proj"},
		{args: []string{"--unsafe-access-host-group=1234"}, consumed: 1, group: "1234"},
		{args: []string{"--unsafe-access-host-group"}, err: true},
		{args: []string{"--unsafe-access-host-group="}, err: true},
		{args: []string{"--node", "n1"}},
	} {
		group := ""
		consumed, err := consumeDaemonAccessOption(test.args, &group)
		if (err != nil) != test.err || consumed != test.consumed || group != test.group {
			t.Errorf("consumeDaemonAccessOption(%q)=(%d, %v) group %q, want (%d, error %t) group %q", test.args, consumed, err, group, test.consumed, test.err, test.group)
		}
	}
}

func TestResolveDaemonAccess(t *testing.T) {
	if access, err := resolveDaemonAccess(""); err != nil || access.shared {
		t.Fatalf("no group=(%+v, %v), want no access", access, err)
	}
	own := strconv.Itoa(os.Getegid())
	access, err := resolveDaemonAccess(own)
	if err != nil || !access.shared || access.gid != os.Getegid() {
		t.Fatalf("own group %s=(%+v, %v)", own, access, err)
	}
	if entry, err := user.LookupGroupId(own); err == nil {
		named, err := resolveDaemonAccess(entry.Name)
		if err != nil || named.gid != os.Getegid() || named.group != entry.Name {
			t.Fatalf("own group by name %q=(%+v, %v)", entry.Name, named, err)
		}
	}
	// A group the test is not in, which a daemon could not give its root to.
	if _, err := resolveDaemonAccess("2147483000"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("foreign group error=%v", err)
	}
	if _, err := resolveDaemonAccess("ttc-no-such-group-for-tests"); err == nil || !strings.Contains(err.Error(), "unknown group") {
		t.Fatalf("unknown group error=%v", err)
	}
}

func TestSecureDaemonDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	shared := daemonAccess{shared: true, group: strconv.Itoa(os.Getegid()), gid: os.Getegid()}
	mode := func() (os.FileMode, int) {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm(), int(info.Sys().(*syscall.Stat_t).Gid)
	}
	if err := secureDaemonDirectory(dir, daemonAccess{}, nil); err != nil {
		t.Fatal(err)
	}
	if perm, _ := mode(); perm != 0o700 {
		t.Fatalf("private root has mode %04o", perm)
	}
	// Shared, the group may search the root and nothing more.
	if err := secureDaemonDirectory(dir, shared, nil); err != nil {
		t.Fatal(err)
	}
	if perm, gid := mode(); perm != 0o710 || gid != os.Getegid() {
		t.Fatalf("shared root has mode %04o and group %d", perm, gid)
	}
	// A daemon started without the option makes it private again.
	var stderr bytes.Buffer
	if err := secureDaemonDirectory(dir, daemonAccess{}, &stderr); err != nil {
		t.Fatal(err)
	}
	if perm, _ := mode(); perm != 0o700 || !strings.Contains(stderr.String(), "private again") {
		t.Fatalf("root after the group's run has mode %04o; stderr %q", perm, stderr.String())
	}
	// It still refuses a root that other users may enter; the option
	// tightens it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureDaemonDirectory(dir, daemonAccess{}, nil); err == nil || !strings.Contains(err.Error(), "allow access by other users") {
		t.Fatalf("world-searchable root error=%v", err)
	}
	if err := secureDaemonDirectory(dir, shared, nil); err != nil {
		t.Fatal(err)
	}
	if perm, _ := mode(); perm != 0o710 {
		t.Fatalf("world-searchable root shared has mode %04o", perm)
	}
}

func TestSecureDaemonSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ttcd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	check := func(access daemonAccess, want os.FileMode) {
		t.Helper()
		if err := secureDaemonSocket(socket, access); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(socket)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("socket mode %04o, want %04o", got, want)
		}
		if gid := int(info.Sys().(*syscall.Stat_t).Gid); access.shared && gid != access.gid {
			t.Fatalf("socket group %d, want %d", gid, access.gid)
		}
	}
	check(daemonAccess{shared: true, gid: os.Getegid()}, 0o660)
	check(daemonAccess{}, 0o600)
}

func TestDaemonAccessAdmits(t *testing.T) {
	owner := os.Geteuid()
	other := owner + 1
	shared := daemonAccess{shared: true, group: "proj", gid: 4242}
	for _, test := range []struct {
		access daemonAccess
		peer   daemonPeer
		want   bool
	}{
		{daemonAccess{}, daemonPeer{UID: owner}, true},
		{daemonAccess{}, daemonPeer{UID: other, GID: 4242}, false},
		{shared, daemonPeer{UID: owner}, true},
		{shared, daemonPeer{UID: other, GID: 4242}, true},
		{shared, daemonPeer{UID: other, GID: 100, Groups: []int{7, 4242}}, true},
		{shared, daemonPeer{UID: other, GID: 100, Groups: []int{7}}, false},
	} {
		if got := test.access.admits(test.peer); got != test.want {
			t.Errorf("%v admits %+v=%t, want %t", test.access, test.peer, got, test.want)
		}
	}
	if err := shared.peerRefusal(daemonPeer{UID: other}); err == nil || !strings.Contains(err.Error(), "nor a member of group proj (GID 4242)") {
		t.Fatalf("refusal=%v", err)
	}
}

func TestRequesterOf(t *testing.T) {
	owner := daemonPeer{UID: os.Geteuid()}
	member := daemonPeer{UID: os.Geteuid() + 1}
	relayed := &daemonRequester{UID: os.Geteuid() + 1}
	if got := requesterOf(owner, nil); !got.Owner || got.UID != os.Geteuid() {
		t.Errorf("owner's own request: %+v", got)
	}
	// The daemon's own user relays a member's request from another daemon.
	if got := requesterOf(owner, relayed); got != *relayed {
		t.Errorf("relayed request: %+v, want %+v", got, *relayed)
	}
	// A member's request is the member's, whatever it says.
	if got := requesterOf(member, &daemonRequester{UID: os.Geteuid(), Owner: true}); got.Owner || got.UID != member.UID {
		t.Errorf("member claiming the owner: %+v", got)
	}
}

func TestOwnerOnlyRefusal(t *testing.T) {
	member := daemonRequester{UID: os.Geteuid() + 1}
	for _, request := range []daemonRequest{
		{Action: "stop"},
		{Action: "stop", Args: []string{"--dehydrate-all"}},
		{Action: "mutation-control", Args: []string{"request", "resolve", "id", "--status", "0"}},
		{Action: "command", Args: []string{"node", "stop", "n3", "--migrate-all"}},
		{Action: "command", Args: []string{"node", "rm", "n3"}},
		{Action: "command", Args: []string{"restore", "-"}},
		{Action: "command", Args: []string{"container", "restore", "--name", "x", "-"}},
	} {
		if err := ownerOnlyRefusal(request, member); err == nil || !strings.Contains(err.Error(), "only the daemon's owner") {
			t.Errorf("member's %s %q: %v", request.Action, request.Args, err)
		}
		if err := ownerOnlyRefusal(request, ownerRequester()); err != nil {
			t.Errorf("owner's %s %q: %v", request.Action, request.Args, err)
		}
	}
	for _, request := range []daemonRequest{
		{Action: "ping"},
		{Action: "mutation-control", Args: []string{"requests"}},
		{Action: "mutation-control", Args: []string{"request", "inspect", "id"}},
		{Action: "command", Args: []string{"node", "ls"}},
		{Action: "command", Args: []string{"stop", "demo@main"}},
	} {
		if err := ownerOnlyRefusal(request, member); err != nil {
			t.Errorf("member's %s %q: %v", request.Action, request.Args, err)
		}
	}
}

func TestMemberHostPathRefusal(t *testing.T) {
	member := daemonRequester{UID: os.Geteuid() + 1}
	for _, test := range []struct {
		args    []string
		refused bool
	}{
		{[]string{"cp", "/home/owner/.ssh/id_rsa", "demo@main:/tmp/"}, true},
		{[]string{"cp", "demo@main:/etc/passwd", "/home/owner/.bashrc"}, true},
		{[]string{"cp", "-a", "--", "demo@main:/x", "./relative"}, true},
		{[]string{"cp", "--ttc-prepared-input", "--", "-", "demo@main:/tmp/"}, false},
		{[]string{"cp", "demo@main:/x", "-"}, false},
		{[]string{"checkpoint", "demo@main", "/home/owner/out.ckpt"}, true},
		{[]string{"checkpoint", "--compression", "none", "demo@main", "-"}, false},
		{[]string{"restore", "--name", "back", "/home/owner/in.ckpt"}, true},
		{[]string{"restore", "-"}, false},
		{[]string{"load", "-i", "/home/owner/image.tar"}, true},
		{[]string{"image", "load", "--input=/home/owner/image.tar"}, true},
		{[]string{"load", "-q"}, false},
		{[]string{"save", "-o", "/home/owner/image.tar", "alpine"}, true},
		{[]string{"image", "save", "alpine"}, false},
		{[]string{"container", "cp", "/etc/shadow", "demo@main:/"}, true},
		{[]string{"run", "-v", "/home/owner:/mnt", "alpine"}, false},
	} {
		err := memberHostPathRefusal(test.args, member)
		if (err != nil) != test.refused {
			t.Errorf("member's %q: %v, want refused %t", test.args, err, test.refused)
		}
		if err := memberHostPathRefusal(test.args, ownerRequester()); err != nil {
			t.Errorf("owner's %q: %v", test.args, err)
		}
	}
}

func TestMemberCreateAndExecRefusals(t *testing.T) {
	member := daemonRequester{UID: os.Geteuid() + 1}
	if err := memberFileRefusal(member, []string{"/home/owner/.env"}, nil); err == nil {
		t.Error("member's environment file was read")
	}
	if err := memberFileRefusal(member, nil, []string{"/home/owner/labels"}); err == nil {
		t.Error("member's label file was read")
	}
	if err := memberFileRefusal(ownerRequester(), []string{"/home/owner/.env"}, []string{"/home/owner/labels"}); err != nil {
		t.Errorf("owner's files: %v", err)
	}
	if err := memberHostMountRefusal(member, 1); err == nil {
		t.Error("member's host bind mount was allowed")
	}
	if err := memberHostMountRefusal(member, 0); err != nil {
		t.Errorf("member without host mounts: %v", err)
	}
	if err := memberHostMountRefusal(ownerRequester(), 2); err != nil {
		t.Errorf("owner's host mounts: %v", err)
	}
	if err := memberExecRefusal(member, "/home/owner/process.json"); err == nil {
		t.Error("member's process file was read")
	}
	if err := memberExecRefusal(ownerRequester(), "/home/owner/process.json"); err != nil {
		t.Errorf("owner's process file: %v", err)
	}
	// A member's variables named without a value never take the daemon's.
	t.Setenv("TTC_GROUP_ACCESS_SECRET", "owner's")
	values, err := collectEnvironment(nil, []string{"TTC_GROUP_ACCESS_SECRET", "SET=1"}, member.environmentLookup())
	if err != nil || values[0] != "TTC_GROUP_ACCESS_SECRET" || values[1] != "SET=1" {
		t.Fatalf("member's environment=(%q, %v)", values, err)
	}
	values, err = collectEnvironment(nil, []string{"TTC_GROUP_ACCESS_SECRET"}, ownerRequester().environmentLookup())
	if err != nil || values[0] != "TTC_GROUP_ACCESS_SECRET=owner's" {
		t.Fatalf("owner's environment=(%q, %v)", values, err)
	}
}

func TestBackendRequesterDefaultsToTheOwner(t *testing.T) {
	if got := backendRequester(&runtimeBackend{}); !got.Owner || got.UID != os.Geteuid() {
		t.Fatalf("backend without a requester: %+v", got)
	}
	member := &daemonRequester{UID: os.Geteuid() + 1}
	backend := &runtimeBackend{requester: member}
	if got := backendRequester(backend); got != *member {
		t.Fatalf("member's backend: %+v", got)
	}
	creator := backend.containerCreator()
	if creator.UID != member.UID || creator.User == "" {
		t.Fatalf("member's container creator: %+v", creator)
	}
	encoded, err := json.Marshal(ttcbundle.ContainerInfo{CreatedBy: creator})
	if err != nil || !strings.Contains(string(encoded), `"created_by":{"uid":`+strconv.Itoa(member.UID)) {
		t.Fatalf("container record=%s, %v", encoded, err)
	}
}

func TestRequesterIsJournaledAndInEvents(t *testing.T) {
	root := t.TempDir()
	id, err := newDaemonRequestID()
	if err != nil {
		t.Fatal(err)
	}
	member := &daemonRequester{UID: os.Geteuid() + 1}
	record, _, err := beginDaemonMutation(root, daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: id,
		Args: []string{"pause", "demo"}, Requester: member,
	})
	if err != nil || record.Requester == nil || *record.Requester != *member {
		t.Fatalf("journaled record=(%+v, %v)", record, err)
	}
	var listing bytes.Buffer
	if status := runDaemonMutationList(root, nil, &listing, &listing); status != 0 || !strings.Contains(listing.String(), "REQUESTER") || !strings.Contains(listing.String(), requesterName(*member)) {
		t.Fatalf("request listing (status %d):\n%s", status, listing.String())
	}

	server := &daemonServer{}
	server.recordCommandEvents(root, []string{"pause", "demo"}, 0, nil, member)
	var output bytes.Buffer
	if err := server.streamEvents(root, []string{"--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var event DaemonEvent
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("events %q: %v", output.String(), err)
	}
	if event.Attributes["requester_uid"] != strconv.Itoa(member.UID) || event.Attributes["requester"] != requesterName(*member) {
		t.Fatalf("event=%+v", event)
	}
}
