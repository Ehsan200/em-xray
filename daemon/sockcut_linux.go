//go:build linux

package daemon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const sockCutSupported = true

// Netlink sock_diag layout (linux/inet_diag.h). x/sys has the constants but
// not the structs.
const (
	nlHdrLen       = 16 // struct nlmsghdr
	diagSockIDLen  = 48 // struct inet_diag_sockid
	diagReqLen     = 8 + diagSockIDLen
	diagMsgMinLen  = 4 + diagSockIDLen
	diagAllStates  = 0xffffffff
	tcpTimeWait    = 6
	tcpClose       = 7
	tcpListen      = 10
	diagRecvBufLen = 1 << 16
)

// cutLoopbackSockets closes every IPv4 TCP and UDP socket whose local or
// remote address is one of addrs, and returns how many it closed. pid is the
// xray process holding them.
//
// First choice is SOCK_DESTROY over NETLINK_SOCK_DIAG (what `ss -K` does): it
// resets a TCP connection, so xray tears down the whole relay behind it. It
// needs CAP_NET_ADMIN and a kernel built with CONFIG_INET_DIAG_DESTROY. A
// kernel without it gets the fallback, shutdownProcSockets.
func cutLoopbackSockets(pid int, addrs []netip.Addr) (int, error) {
	want := map[[4]byte]bool{}
	for _, a := range addrs {
		if a.Is4() {
			want[a.As4()] = true
		}
	}
	if len(want) == 0 {
		return 0, nil
	}
	return cutSockets(pid, func(src, dst netip.AddrPort) bool {
		return want[src.Addr().As4()] || want[dst.Addr().As4()]
	})
}

// cutRemoteEndpoints closes every IPv4 TCP and UDP socket connected to one of
// eps (remote address and port), and returns how many it closed. pid is the
// xray process holding them. Same mechanism as cutLoopbackSockets.
func cutRemoteEndpoints(pid int, eps []netip.AddrPort) (int, error) {
	want := map[netip.AddrPort]bool{}
	for _, ep := range eps {
		if ep.Addr().Is4() {
			want[ep] = true
		}
	}
	if len(want) == 0 {
		return 0, nil
	}
	return cutSockets(pid, func(_, dst netip.AddrPort) bool { return want[dst] })
}

// sockMatch selects sockets by their local (src) and remote (dst) endpoint.
type sockMatch func(src, dst netip.AddrPort) bool

func cutSockets(pid int, match sockMatch) (int, error) {
	n, err := destroySockets(match)
	if errors.Is(err, unix.EOPNOTSUPP) && pid > 0 {
		return shutdownProcSockets(pid, match)
	}
	return n, err
}

// destroySockets destroys the sockets match selects via sock_diag.
func destroySockets(match sockMatch) (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, fmt.Errorf("netlink bind: %w", err)
	}
	// The caller holds the supervisor lock: a kernel that never answers must
	// not wedge every reconcile behind it.
	tv := unix.Timeval{Sec: 2}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return 0, err
	}
	nl := &diagConn{fd: fd}
	n := 0
	for _, proto := range []uint8{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
		ids, err := nl.dump(proto, match)
		if err != nil {
			return n, err
		}
		for _, id := range ids {
			switch err := nl.destroy(proto, id); {
			case err == nil:
				n++
			case errors.Is(err, unix.ENOENT):
				// closed on its own since the dump
			default:
				return n, err
			}
		}
	}
	return n, nil
}

// shutdownProcSockets is the fallback for kernels without socket destroy: it
// finds pid's sockets match selects (/proc/<pid>/net + /proc/<pid>/fd), takes
// a copy of each descriptor with pidfd_getfd and shuts it down. The socket is
// shared, so xray reads EOF and tears the relay down just the same. Needs
// Linux 5.6+ and ptrace rights over pid, which the daemon has as xray's
// parent running as root.
func shutdownProcSockets(pid int, match sockMatch) (int, error) {
	inodes := map[string]bool{}
	for _, proto := range []string{"tcp", "udp"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, proto))
		if err != nil {
			return 0, err
		}
		for ino := range procNetInodes(string(b), proto == "tcp", match) {
			inodes[ino] = true
		}
	}
	if len(inodes) == 0 {
		return 0, nil
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return 0, fmt.Errorf("pidfd_open: %w", err)
	}
	defer unix.Close(pidfd)
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range fds {
		link, err := os.Readlink(dir + "/" + e.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") || !inodes[strings.TrimSuffix(link[len("socket:["):], "]")] {
			continue
		}
		target, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fd, err := unix.PidfdGetfd(pidfd, target, 0)
		if err != nil {
			if errors.Is(err, unix.EBADF) {
				continue // closed on its own meanwhile
			}
			return n, fmt.Errorf("pidfd_getfd: %w", err)
		}
		// ENOTCONN on an unconnected UDP socket still wakes its readers.
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		_ = unix.Close(fd)
		n++
	}
	return n, nil
}

// procNetInodes returns the socket inodes of a /proc/net/{tcp,udp} table that
// match selects, skipping TCP sockets with nothing open to cut.
func procNetInodes(table string, tcp bool, match sockMatch) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		if tcp {
			if st, err := strconv.ParseUint(f[3], 16, 8); err != nil || st == tcpTimeWait || st == tcpClose || st == tcpListen {
				continue
			}
		}
		src, ok1 := procNetAddr(f[1])
		dst, ok2 := procNetAddr(f[2])
		if ok1 && ok2 && match(src, dst) {
			out[f[9]] = true
		}
	}
	return out
}

// procNetAddr parses a /proc/net "0100007F:1F90" endpoint: the IPv4 address
// is the in-memory (network order) word printed as a native integer, the port
// a plain hex number.
func procNetAddr(s string) (netip.AddrPort, bool) {
	host, port, ok := strings.Cut(s, ":")
	if !ok || len(host) != 8 {
		return netip.AddrPort{}, false
	}
	v, err := strconv.ParseUint(host, 16, 32)
	if err != nil {
		return netip.AddrPort{}, false
	}
	p, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	var a [4]byte
	binary.NativeEndian.PutUint32(a[:], uint32(v))
	return netip.AddrPortFrom(netip.AddrFrom4(a), uint16(p)), true
}

type diagConn struct {
	fd  int
	seq uint32
}

// request sends one sock_diag message and returns its sequence number.
func (c *diagConn) request(typ, flags uint16, proto uint8, id []byte) (uint32, error) {
	c.seq++
	b := make([]byte, nlHdrLen+diagReqLen)
	binary.NativeEndian.PutUint32(b[0:], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:], typ)
	binary.NativeEndian.PutUint16(b[6:], flags)
	binary.NativeEndian.PutUint32(b[8:], c.seq)
	req := b[nlHdrLen:]
	req[0] = unix.AF_INET
	req[1] = proto
	binary.NativeEndian.PutUint32(req[4:], diagAllStates)
	copy(req[8:], id)
	if err := unix.Sendto(c.fd, b, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, fmt.Errorf("netlink send: %w", err)
	}
	return c.seq, nil
}

// each reads the replies to seq, calling fn with every data message, until the
// dump ends or the ack arrives. A netlink error reply is returned as its errno.
func (c *diagConn) each(seq uint32, fn func(data []byte)) error {
	buf := make([]byte, diagRecvBufLen)
	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return fmt.Errorf("netlink recv: %w", err)
		}
		for b := buf[:n]; len(b) >= nlHdrLen; {
			l := int(binary.NativeEndian.Uint32(b[0:]))
			if l < nlHdrLen || l > len(b) {
				return errors.New("netlink: malformed reply")
			}
			typ := binary.NativeEndian.Uint16(b[4:])
			msgSeq := binary.NativeEndian.Uint32(b[8:])
			data := b[nlHdrLen:l]
			if next := (l + 3) &^ 3; next < len(b) {
				b = b[next:]
			} else {
				b = nil
			}
			if msgSeq != seq {
				continue
			}
			switch typ {
			case unix.NLMSG_DONE:
				return nil
			case unix.NLMSG_ERROR:
				if len(data) < 4 {
					return errors.New("netlink: short error reply")
				}
				if e := int32(binary.NativeEndian.Uint32(data)); e != 0 {
					return unix.Errno(-e)
				}
				return nil // the ack
			default:
				fn(data)
			}
		}
	}
}

// dump returns the socket ids of every live proto socket match selects.
func (c *diagConn) dump(proto uint8, match sockMatch) ([][]byte, error) {
	seq, err := c.request(unix.SOCK_DIAG_BY_FAMILY, unix.NLM_F_REQUEST|unix.NLM_F_DUMP, proto, nil)
	if err != nil {
		return nil, err
	}
	var ids [][]byte
	err = c.each(seq, func(m []byte) {
		if len(m) < diagMsgMinLen || m[0] != unix.AF_INET {
			return
		}
		if proto == unix.IPPROTO_TCP {
			switch m[1] {
			case tcpTimeWait, tcpClose, tcpListen:
				return // nothing open to cut
			}
		}
		id := m[4 : 4+diagSockIDLen]
		// inet_diag_sockid: sport, dport (big endian), src[16], dst[16].
		var src, dst [4]byte
		copy(src[:], id[4:8])
		copy(dst[:], id[20:24])
		sp := netip.AddrPortFrom(netip.AddrFrom4(src), binary.BigEndian.Uint16(id[0:2]))
		dp := netip.AddrPortFrom(netip.AddrFrom4(dst), binary.BigEndian.Uint16(id[2:4]))
		if match(sp, dp) {
			ids = append(ids, append([]byte(nil), id...))
		}
	})
	return ids, err
}

// destroy closes the socket with the given id.
func (c *diagConn) destroy(proto uint8, id []byte) error {
	seq, err := c.request(unix.SOCK_DESTROY, unix.NLM_F_REQUEST|unix.NLM_F_ACK, proto, id)
	if err != nil {
		return err
	}
	return c.each(seq, func([]byte) {})
}
