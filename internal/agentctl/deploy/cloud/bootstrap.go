package cloud

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHConfig loads a private key and returns an ssh.ClientConfig suitable for
// dialing the freshly-bootstrapped host. We accept the host key on first use
// (InsecureIgnoreHostKey) — the alternative is to scan /etc/ssh/ssh_host_* via
// the cloud provider's console-output API, which is fragile and provider-
// specific. The trust anchor here is the cloud control-plane: the IP belongs
// to the instance we just provisioned with our user-data, not an attacker.
//
// For long-lived bare-metal targets a future TODO is to record the host key on
// first connect and pin it on subsequent runs.
func SSHConfig(user, keyPath string) (*ssh.ClientConfig, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w", keyPath, err)
	}
	return &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // see doc comment above
		Timeout:         15 * time.Second,
	}, nil
}

// WaitForSentinel polls SSH `test -f <path>` until the file exists or the
// context fires. Returns the first successful connect's wait+exit time.
//
// Retries any dial/exec error (the host may still be coming up; sshd may not
// be ready; cloud-init may not have run the script yet). A non-zero exit from
// `test -f` is treated as "not ready yet" and retried.
func WaitForSentinel(ctx context.Context, addr string, cfg *ssh.ClientConfig, sentinel string, every time.Duration) error {
	if every <= 0 {
		every = 5 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait sentinel %s: %w", sentinel, err)
		}
		ok, err := remoteFileExists(ctx, addr, cfg, sentinel)
		if err == nil && ok {
			return nil
		}
		// err == nil && !ok -> not yet; err != nil -> transient (ssh still warming up)
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait sentinel %s: %w", sentinel, ctx.Err())
		case <-time.After(every):
		}
	}
}

// FetchFile reads a remote file's contents over SSH. The remote read uses
// `cat` so any reasonable user (root or one with sudo + nopasswd) can pull
// /root/admin.conf and friends.
func FetchFile(ctx context.Context, addr string, cfg *ssh.ClientConfig, path string) ([]byte, error) {
	out, _, err := runSSH(ctx, addr, cfg, "cat "+shellQuote(path))
	if err != nil {
		return nil, fmt.Errorf("ssh cat %s: %w", path, err)
	}
	return out, nil
}

// remoteFileExists returns (true, nil) if `test -f path` exits 0, (false, nil)
// for non-zero exit, or (false, err) if the SSH session itself fails.
func remoteFileExists(ctx context.Context, addr string, cfg *ssh.ClientConfig, path string) (bool, error) {
	_, _, err := runSSH(ctx, addr, cfg, "test -f "+shellQuote(path))
	if err == nil {
		return true, nil
	}
	var exitErr *ssh.ExitError
	if errAs(err, &exitErr) {
		return false, nil // non-zero exit: file not there yet
	}
	return false, err
}

// runSSH dials addr, opens a session, runs cmd, and returns (stdout, stderr, err).
// Honors ctx by closing the connection on cancellation.
func runSSH(ctx context.Context, addr string, cfg *ssh.ClientConfig, cmd string) ([]byte, []byte, error) {
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	defer rawConn.Close()

	conn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	go func() {
		<-ctx.Done()
		_ = client.Close()
	}()

	sess, err := client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if err := sess.Run(cmd); err != nil {
		return stdout.Bytes(), stderr.Bytes(), err
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// errAs is errors.As without dragging the import here for a single use.
func errAs(err error, target **ssh.ExitError) bool {
	if err == nil {
		return false
	}
	for e := err; e != nil; {
		if x, ok := e.(*ssh.ExitError); ok {
			*target = x
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// shellQuote single-quotes a string so it's safe as a single shell argument.
func shellQuote(s string) string {
	var b bytes.Buffer
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' {
			b.WriteString(`'\''`)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// CopyTo streams r to the remote file at path (best-effort; uses `cat >`).
// Not used in V1 but kept here for the baremetal target's eventual k0s
// bootstrap script upload.
func CopyTo(ctx context.Context, addr string, cfg *ssh.ClientConfig, path string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer rawConn.Close()
	conn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, cfg)
	if err != nil {
		return err
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	return sess.Run("cat > " + shellQuote(path))
}
