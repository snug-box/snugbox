//go:build linux

package helperapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// Peer is the process at the other end of a connection.
type Peer struct {
	UID, GID, PID int
}

func peerOf(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if serr != nil {
		return nil, fmt.Errorf("SO_PEERCRED: %w", serr)
	}
	return cred, nil
}

// ErrFDs means a client tried to pass file descriptors, which the helper
// never accepts.
var ErrFDs = errors.New("helperapi: the message carries file descriptors, which the helper refuses")

// ErrSender means a message came from a process other than the one that
// opened the connection, or carried no sender at all.
var ErrSender = errors.New("helperapi: the message was not sent by the process that opened the connection")

// Conn is one connection the helper accepted. Every read checks the
// credentials the kernel attached to that data.
type Conn struct {
	uc   *net.UnixConn
	Peer Peer
	r    *bufio.Reader
}

// credReader reads with recvmsg and checks each chunk's SCM_CREDENTIALS
// against the connection's owner. Unix stream sockets never merge data from
// senders with different credentials into one read, so each chunk has one
// sender.
type credReader struct {
	uc  *net.UnixConn
	uid uint32
	oob []byte
}

func (c *credReader) Read(p []byte) (int, error) {
	n, oobn, flags, _, err := c.uc.ReadMsgUnix(p, c.oob)
	if oobn > 0 {
		if cerr := c.check(c.oob[:oobn]); cerr != nil {
			return 0, cerr
		}
	} else if n > 0 {
		return 0, ErrSender
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return 0, ErrFDs
	}
	return n, err
}

func (c *credReader) check(oob []byte) error {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return fmt.Errorf("helperapi: control message: %w", err)
	}
	sender := ErrSender
	for i := range msgs {
		m := &msgs[i]
		if m.Header.Level != unix.SOL_SOCKET {
			return fmt.Errorf("helperapi: unexpected control message level %d", m.Header.Level)
		}
		switch m.Header.Type {
		case unix.SCM_RIGHTS:
			fds, _ := unix.ParseUnixRights(m)
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
			return ErrFDs
		case unix.SCM_CREDENTIALS:
			cred, err := unix.ParseUnixCredentials(m)
			if err == nil && cred.Uid == c.uid && cred.Pid != 0 {
				sender = nil
			}
		default:
			return fmt.Errorf("helperapi: unexpected control message type %d", m.Header.Type)
		}
	}
	return sender
}

// PassCredentials turns on SO_PASSCRED for a listening socket, so the kernel
// attaches the sender to every message of every connection it accepts.
// systemd sets it too, from PassCredentials=yes in the socket unit.
func PassCredentials(ln *net.UnixListener) error {
	raw, err := ln.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1)
	}); err != nil {
		return err
	}
	return serr
}

// Accept waits for the next connection and reads its caller.
func Accept(ln *net.UnixListener) (*Conn, error) {
	uc, err := ln.AcceptUnix()
	if err != nil {
		return nil, err
	}
	cred, err := peerOf(uc)
	if err != nil {
		return nil, errors.Join(err, uc.Close())
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, errors.Join(err, uc.Close())
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1)
	}); err != nil || serr != nil {
		return nil, errors.Join(err, serr, uc.Close())
	}
	cr := &credReader{uc: uc, uid: cred.Uid, oob: make([]byte, unix.CmsgSpace(unix.SizeofUcred)+unix.CmsgSpace(4*8))}
	peer := Peer{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}
	return &Conn{uc: uc, Peer: peer, r: bufio.NewReaderSize(cr, 4096)}, nil
}

// Reader returns the connection's checked reader.
func (c *Conn) Reader() *bufio.Reader { return c.r }

// Write sends bytes to the client.
func (c *Conn) Write(b []byte) (int, error) { return c.uc.Write(b) }

// SetReadDeadline bounds how long the helper waits for a request.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.uc.SetReadDeadline(t) }

// SetWriteDeadline bounds how long the helper waits for a client to take an
// answer, so a client that stops reading can't hold it.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.uc.SetWriteDeadline(t) }

// Close closes the connection.
func (c *Conn) Close() error { return c.uc.Close() }

// ActivationListener returns the socket systemd passed as file descriptor
// 3, checking LISTEN_PID and LISTEN_FDS as sd_listen_fds does.
func ActivationListener() (*net.UnixListener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, errors.New("not started by systemd socket activation: LISTEN_PID is not this process")
	}
	if os.Getenv("LISTEN_FDS") != "1" {
		return nil, fmt.Errorf("systemd passed %q sockets, want exactly 1", os.Getenv("LISTEN_FDS"))
	}
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if err := os.Unsetenv(v); err != nil {
			return nil, err
		}
	}
	const first = 3
	unix.CloseOnExec(first)
	f := os.NewFile(first, "helper.sock")
	ln, err := net.FileListener(f)
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("socket from systemd: %w", err)
	}
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		return nil, errors.Join(errors.New("the socket from systemd is not a Unix socket"), ln.Close())
	}
	return ul, nil
}

// dial connects to the helper and checks that root owns the listening end,
// so a process that replaced the socket can't pose as the helper. A test
// server listens as its own user and passes that ID instead.
func dial(ctx context.Context, socket string, serverUID int) (*net.UnixConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, errors.Join(errors.New("helperapi: not a Unix socket"), c.Close())
	}
	cred, err := peerOf(uc)
	if err != nil {
		return nil, errors.Join(err, uc.Close())
	}
	if int64(cred.Uid) != int64(serverUID) {
		return nil, errors.Join(fmt.Errorf("helperapi: %s is held by user %d, not the helper; refusing to talk to it", socket, cred.Uid), uc.Close())
	}
	return uc, nil
}
