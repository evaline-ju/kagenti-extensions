package cluster

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestFreeLocalPortReturnsUsablePort(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port out of range: %d", port)
	}
	// We should be able to listen on it (the listener inside freeLocalPort
	// has been closed by now).
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatalf("could not bind to returned port %d: %v", port, err)
	}
	l.Close()
}

func TestWaitForAcceptReturnsWhenListenerOpens(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	// Open a listener on the port after a short delay; waitForAccept
	// should return nil once the dial succeeds.
	go func() {
		time.Sleep(50 * time.Millisecond)
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err == nil {
			defer l.Close()
			// Accept one connection from waitForAccept and close.
			c, _ := l.Accept()
			if c != nil {
				c.Close()
			}
			time.Sleep(100 * time.Millisecond) // give waitForAccept room to return
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitForAccept(ctx, port); err != nil {
		t.Fatalf("waitForAccept: %v", err)
	}
}

func TestWaitForAcceptRespectsContext(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	// No listener — waitForAccept should return ctx.Err() once we cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := waitForAccept(ctx, port); err == nil {
		t.Fatal("want context-deadline error, got nil")
	}
}

// itoa is a tiny helper to avoid importing strconv in the test file just
// for one call.
func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
