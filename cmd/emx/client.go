package main

import (
	"context"
	"fmt"
	"net"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/ehsan200/em-xray/internal/xraybin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func xrayEmbeddedVersion() string { return xraybin.Version() }

// dial connects to the running daemon over its unix socket.
func dial(ctx context.Context) (emxv1.DaemonClient, *grpc.ClientConn, error) {
	sock := paths.Default().Socket()
	conn, err := grpc.NewClient(
		"unix:"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}),
	)
	if err != nil {
		return nil, nil, err
	}
	return emxv1.NewDaemonClient(conn), conn, nil
}

// dialReady dials and confirms the daemon answers a Ping within the deadline.
func dialReady(ctx context.Context) (emxv1.DaemonClient, *grpc.ClientConn, error) {
	c, conn, err := dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.Ping(ctx, &emxv1.PingRequest{}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return c, conn, nil
}

// waitReady polls Ping until the daemon answers or the timeout elapses.
func waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		c, conn, err := dial(ctx)
		if err == nil {
			_, err = c.Ping(ctx, &emxv1.PingRequest{})
			conn.Close()
		}
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not become ready within %s: %w", timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
