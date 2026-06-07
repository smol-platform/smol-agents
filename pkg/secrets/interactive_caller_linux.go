//go:build linux

package secrets

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// PeerCallerClass classifies a UDS peer by walking its process ancestry from the
// SO_PEERCRED PID (M4.12). Wires Server.ClassifyConn. Fails open to CallerAgent
// when the PID or /proc is unreadable — the broker's other controls still apply.
func PeerCallerClass(conn net.Conn, anc ProcAncestry) CallerClass {
	pid, err := peerPID(conn)
	if err != nil {
		return CallerAgent
	}
	chain, err := anc.Ancestry(pid)
	if err != nil {
		return CallerAgent
	}
	return ClassifyAncestry(chain)
}

// ProcfsAncestry reads /proc to build a caller's process-ancestry comm chain.
type ProcfsAncestry struct{}

// Ancestry returns the comm chain from pid up toward PID 1 (caller first),
// bounded to avoid a cycle wedging the broker.
func (ProcfsAncestry) Ancestry(pid int) ([]string, error) {
	var chain []string
	for i := 0; i < 32 && pid > 1; i++ {
		comm, ppid, err := procStat(pid)
		if err != nil {
			return chain, err
		}
		chain = append(chain, comm)
		if ppid == pid { // defensive: never loop
			break
		}
		pid = ppid
	}
	return chain, nil
}

// procStat parses /proc/<pid>/stat for (comm, ppid). comm is parenthesized and
// may contain spaces/parens, so we split on the LAST ')'.
func procStat(pid int) (string, int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	s := string(b)
	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close < 0 || close < open {
		return "", 0, fmt.Errorf("secrets: malformed /proc/%d/stat", pid)
	}
	comm := s[open+1 : close]
	fields := strings.Fields(s[close+1:]) // state ppid pgrp ...
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("secrets: short /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, err
	}
	return comm, ppid, nil
}
