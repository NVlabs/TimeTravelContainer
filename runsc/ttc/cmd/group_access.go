// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// unsafeAccessHostGroupOption shares a daemon with the members of a Unix
// group. A member can do through the daemon whatever the daemon's owner, the
// user it runs as, can: the daemon runs a member's containers, and every
// container is the group's. It gives a member no host path and no part of its
// own environment, but a member's container still runs as the owner, so the
// group should trust each other as much as the owner: hence "unsafe".
const unsafeAccessHostGroupOption = "--unsafe-access-host-group"

// daemonAccess says who may use a daemon besides its owner: no one, or with
// --unsafe-access-host-group, the members of one Unix group.
type daemonAccess struct {
	shared bool
	// group is the group as the option named it, and gid its ID.
	group string
	gid   int
}

func (a daemonAccess) String() string {
	if !a.shared {
		return "its owner"
	}
	if a.group == strconv.Itoa(a.gid) {
		return fmt.Sprintf("group %d", a.gid)
	}
	return fmt.Sprintf("group %s (GID %d)", a.group, a.gid)
}

// status is what 'ttc daemon status' reports of access: nothing for a
// daemon that only its owner uses, and the group of one it is shared with.
func (a daemonAccess) status() any {
	if !a.shared {
		return nil
	}
	return map[string]any{"group": a.group, "gid": a.gid}
}

// consumeDaemonAccessOption consumes --unsafe-access-host-group GROUP, which
// sets *group.
func consumeDaemonAccessOption(args []string, group *string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if args[0] == unsafeAccessHostGroupOption {
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return 0, fmt.Errorf("%s requires a group name or ID", unsafeAccessHostGroupOption)
		}
		*group = args[1]
		return 2, nil
	}
	if value, ok := strings.CutPrefix(args[0], unsafeAccessHostGroupOption+"="); ok {
		if strings.TrimSpace(value) == "" {
			return 0, fmt.Errorf("%s requires a group name or ID", unsafeAccessHostGroupOption)
		}
		*group = value
		return 1, nil
	}
	return 0, nil
}

// resolveDaemonAccess resolves the group --unsafe-access-host-group names,
// which this daemon's user must belong to: the daemon gives its root and its
// socket to the group, as chown(2) lets a user do only for its own groups.
func resolveDaemonAccess(group string) (daemonAccess, error) {
	if group == "" {
		return daemonAccess{}, nil
	}
	gid, err := lookupGroupID(group)
	if err != nil {
		return daemonAccess{}, err
	}
	access := daemonAccess{shared: true, group: group, gid: gid}
	if !processInGroup(gid) {
		return daemonAccess{}, fmt.Errorf("%s: the daemon's user, UID %d, is not a member of %s in this session; a user added to a group since it logged in can start the daemon under 'newgrp %s'", unsafeAccessHostGroupOption, os.Geteuid(), access, group)
	}
	return access, nil
}

// lookupGroupID resolves a group's ID, which may be given as such. Go's own
// lookup reads /etc/group alone when it does not use the C library, so a name
// it does not know is asked of getent, which asks NSS, and so LDAP and the
// like, as the system does.
func lookupGroupID(group string) (int, error) {
	if id, err := strconv.Atoi(group); err == nil {
		if id < 0 {
			return 0, fmt.Errorf("invalid group ID %d", id)
		}
		return id, nil
	}
	if entry, err := user.LookupGroup(group); err == nil {
		if id, err := strconv.Atoi(entry.Gid); err == nil {
			return id, nil
		}
	}
	if output, err := exec.Command("getent", "group", group).Output(); err == nil {
		fields := strings.Split(strings.TrimSpace(string(output)), ":")
		if len(fields) >= 3 && fields[0] == group {
			if id, err := strconv.Atoi(fields[2]); err == nil && id >= 0 {
				return id, nil
			}
		}
	}
	return 0, fmt.Errorf("unknown group %q; name it by its ID instead", group)
}

// processInGroup reports whether this process belongs to group gid.
func processInGroup(gid int) bool {
	if os.Getegid() == gid {
		return true
	}
	groups, err := os.Getgroups()
	return err == nil && slices.Contains(groups, gid)
}

// accessGroupReachWarning names the first directory above root that members
// of group gid might not search, and so not reach the daemon's socket in
// root, judged by permission bits alone: an ACL may still let them.
func accessGroupReachWarning(root string, gid int) string {
	for dir := filepath.Dir(root); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				mode := info.Mode().Perm()
				if mode&0o001 == 0 && (mode&0o010 == 0 || int(stat.Gid) != gid) {
					return fmt.Sprintf("members of group %d may not be able to reach the daemon's socket: %s, owned by UID %d and group %d, has permissions %04o", gid, dir, stat.Uid, stat.Gid, mode)
				}
			}
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}

// secureDaemonDirectory makes dir, the daemon's root, which holds its socket,
// private to the daemon's user. With a shared access, it gives dir to the
// group too, but only to search: a member then reaches the socket and nothing
// else in it, and can neither read what the daemon keeps there nor put a
// socket of its own in its place. A root that a daemon shared with a group
// before becomes private again.
func secureDaemonDirectory(dir string, access daemonAccess, stderr io.Writer) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket parent %q is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("socket directory %q is owned by UID %d, not %d", dir, stat.Uid, os.Geteuid())
	}
	perm := info.Mode().Perm()
	if access.shared {
		if err := os.Lchown(dir, -1, access.gid); err != nil {
			return fmt.Errorf("giving the daemon's root %q to %s: %w", dir, access, err)
		}
		if perm != 0o710 {
			if err := os.Chmod(dir, 0o710); err != nil {
				return fmt.Errorf("letting %s search the daemon's root %q: %w", access, dir, err)
			}
		}
		if warning := accessGroupReachWarning(dir, access.gid); warning != "" && stderr != nil {
			fmt.Fprintf(stderr, "ttcd: warning: %s\n", warning)
		}
		return nil
	}
	if perm&0o007 != 0 {
		return fmt.Errorf("the daemon's root %q, which holds its socket, has permissions %04o that allow access by other users; make it private with chmod 700", dir, perm)
	}
	if perm&0o070 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("making the daemon's root %q private again: %w", dir, err)
		}
		if stderr != nil {
			fmt.Fprintf(stderr, "ttcd: the daemon's root %s had permissions %04o, which shared it with a group; it is private again, as the daemon started without %s\n", dir, perm, unsafeAccessHostGroupOption)
		}
	}
	return nil
}

// secureDaemonSocket gives the daemon's socket to its user, and with a shared
// access, to the group as well.
func secureDaemonSocket(socket string, access daemonAccess) error {
	if access.shared {
		if err := os.Lchown(socket, -1, access.gid); err != nil {
			return fmt.Errorf("giving the daemon's socket to %s: %w", access, err)
		}
		return os.Chmod(socket, 0o660)
	}
	return os.Chmod(socket, 0o600)
}

// daemonPeer is a client of the daemon's socket, as the kernel names it.
type daemonPeer struct {
	UID    int
	GID    int
	Groups []int
}

// admits reports whether access lets peer use the daemon: its user always,
// and with a shared access, a member of the group.
func (a daemonAccess) admits(peer daemonPeer) bool {
	if peer.UID == os.Geteuid() {
		return true
	}
	return a.shared && (peer.GID == a.gid || slices.Contains(peer.Groups, a.gid))
}

// peerRefusal is what a daemon tells itself of a peer it does not admit.
func (a daemonAccess) peerRefusal(peer daemonPeer) error {
	if !a.shared {
		return fmt.Errorf("daemon peer UID %d does not match server UID %d", peer.UID, os.Geteuid())
	}
	return fmt.Errorf("daemon peer UID %d is neither the daemon's user, UID %d, nor a member of %s", peer.UID, os.Geteuid(), a)
}

// daemonRequester is who asked for a request: the daemon's owner, the user
// it runs as, or a member of the group it is shared with. The daemon a client
// connects to names the client from its credentials, whatever the request
// says, and keeps that in the request for the daemons it forwards it to:
// those hear it from their own user, the cluster's daemons, and keep what it
// says.
type daemonRequester struct {
	UID   int  `json:"uid"`
	Owner bool `json:"owner"`
}

// requesterOf names who asked for a request that peer sent, which says that
// claimed did.
func requesterOf(peer daemonPeer, claimed *daemonRequester) daemonRequester {
	if peer.UID == os.Geteuid() {
		if claimed != nil {
			return *claimed
		}
		return daemonRequester{UID: peer.UID, Owner: true}
	}
	return daemonRequester{UID: peer.UID}
}

// ownerRequester is the requester of what the daemon does on its own, and of
// a request that names none.
func ownerRequester() daemonRequester {
	return daemonRequester{UID: os.Geteuid(), Owner: true}
}

var userNames sync.Map

// requesterName is the name of the requester's user, if its host knows it,
// and its UID otherwise.
func requesterName(requester daemonRequester) string {
	if name, ok := userNames.Load(requester.UID); ok {
		return name.(string)
	}
	name := strconv.Itoa(requester.UID)
	if entry, err := user.LookupId(name); err == nil && entry.Username != "" {
		name = entry.Username
	} else if output, err := exec.Command("getent", "passwd", name).Output(); err == nil {
		// NSS knows users, as of LDAP, that Go's own lookup does not.
		if fields := strings.Split(strings.TrimSpace(string(output)), ":"); len(fields) >= 3 && fields[0] != "" && fields[2] == name {
			name = fields[0]
		}
	}
	userNames.Store(requester.UID, name)
	return name
}

// ownerOnlyRefusal refuses a member what only the daemon's owner may do:
// stop it, stop or forget a node of its cluster, resolve an interrupted
// request, and restore a checkpoint, whose container may bind mount host
// paths, which the daemon would mount as its owner: a member could have
// written the archive.
func ownerOnlyRefusal(request daemonRequest, requester daemonRequester) error {
	if requester.Owner {
		return nil
	}
	refuse := func(what string) error {
		return fmt.Errorf("only the daemon's owner, UID %d, may %s; UID %d is a member of the group it is shared with", os.Geteuid(), what, requester.UID)
	}
	switch request.Action {
	case "stop":
		return refuse("stop it")
	case "mutation-control":
		if len(request.Args) >= 2 && request.Args[0] == "request" && request.Args[1] == "resolve" {
			return refuse("resolve an interrupted request")
		}
	case "command":
		args := request.Args
		if len(args) != 0 && args[0] == "container" {
			args = args[1:]
		}
		if len(args) >= 2 && args[0] == "node" && (args[1] == "stop" || args[1] == "rm") {
			return refuse("stop or forget a node")
		}
		if len(args) != 0 && args[0] == "restore" {
			return refuse("restore a checkpoint, whose container may bind mount host paths")
		}
	}
	return nil
}

// memberHostPathRefusal refuses a member a command that would have the daemon
// open a host path: the daemon would open it as its owner. A member's ttc
// streams such files itself, and passes the daemon '-' instead.
func memberHostPathRefusal(args []string, requester daemonRequester) error {
	if requester.Owner || len(args) == 0 {
		return nil
	}
	refuse := func(what string) error {
		return fmt.Errorf("ttcd opens no host path for a member of the group it is shared with, as it would open it as its owner: %s; pass '-' and stream it instead, as ttc does", what)
	}
	command, rest := args[0], args[1:]
	if (command == "container" || command == "image") && len(rest) != 0 {
		command, rest = rest[0], rest[1:]
	}
	switch command {
	case "cp":
		_, source, destination, err := parseCopyCommandArgs(rest)
		if err != nil {
			return err
		}
		for _, operand := range []string{source, destination} {
			if _, container, err := splitCopyContainerEndpoint(operand); err != nil {
				return err
			} else if !container && operand != "-" {
				return refuse(fmt.Sprintf("cp's host path %q", operand))
			}
		}
	case "checkpoint", "restore":
		if len(rest) != 0 && rest[len(rest)-1] != "-" {
			return refuse(fmt.Sprintf("%s's file %q", command, rest[len(rest)-1]))
		}
	case "load":
		if name, ok := namedFileOption(rest, "-i", "--input"); ok {
			return refuse(fmt.Sprintf("load's input %q", name))
		}
	case "save":
		if name, ok := namedFileOption(rest, "-o", "--output"); ok {
			return refuse(fmt.Sprintf("save's output %q", name))
		}
	}
	return nil
}

// namedFileOption returns the file a short or long option names in args.
func namedFileOption(args []string, short, long string) (string, bool) {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if (arg == short || arg == long) && i+1 < len(args) {
			return args[i+1], true
		}
		for _, name := range []string{short, long} {
			if value, ok := strings.CutPrefix(arg, name+"="); ok {
				return value, true
			}
		}
	}
	return "", false
}

// memberFileRefusal refuses a member a run or a create that names an
// environment or a label file, which the daemon would open as its owner. A
// member's ttc reads them itself, and passes what they say instead.
func memberFileRefusal(requester daemonRequester, envFiles, labelFiles []string) error {
	if requester.Owner || len(envFiles)+len(labelFiles) == 0 {
		return nil
	}
	return errors.New("ttcd reads no environment or label file for a member of the group it is shared with, as it would open it as its owner; ttc reads them itself")
}

// memberHostMountRefusal refuses a member a run or a create that bind mounts
// host paths, which the daemon would open as its owner.
func memberHostMountRefusal(requester daemonRequester, hostMounts int) error {
	if requester.Owner || hostMounts == 0 {
		return nil
	}
	return errors.New("a member of the group the daemon is shared with may not bind mount a host path, which the daemon would open as its owner; use a TTC volume instead")
}

// environmentLookup fills an environment variable that a create names
// without a value: from the daemon's environment for its owner, and for a
// member, whose ttc filled it from its own if it could, from nothing.
func (r daemonRequester) environmentLookup() func(string) (string, bool) {
	if r.Owner {
		return os.LookupEnv
	}
	return func(string) (string, bool) { return "", false }
}

// backendRequester is who asked for the command that backend runs.
func backendRequester(backend backend) daemonRequester {
	if runtime, ok := backend.(*runtimeBackend); ok && runtime.requester != nil {
		return *runtime.requester
	}
	return ownerRequester()
}

// memberExecRefusal refuses a member an exec whose process the daemon would
// read from a host file as its owner.
func memberExecRefusal(requester daemonRequester, processPath string) error {
	if requester.Owner || processPath == "" {
		return nil
	}
	return errors.New("--process names a file that ttcd would open as its owner, which it does not for a member of the group it is shared with; give the process as options and arguments instead")
}

// containerCreator names who asks for a container this backend creates.
func (b *runtimeBackend) containerCreator() *ttcbundle.ContainerCreator {
	requester := backendRequester(b)
	return &ttcbundle.ContainerCreator{UID: requester.UID, User: requesterName(requester)}
}
