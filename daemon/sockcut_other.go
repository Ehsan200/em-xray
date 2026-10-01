//go:build !linux

package daemon

import "net/netip"

// Only Linux can destroy another process's sockets (and bind the per-master
// loopback addresses at all); elsewhere old connections end at the idle
// timeout.
const sockCutSupported = false

func cutLoopbackSockets(int, []netip.Addr) (int, error) { return 0, nil }

func cutRemoteEndpoints(int, []netip.AddrPort) (int, error) { return 0, nil }
