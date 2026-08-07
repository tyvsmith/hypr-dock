package wl

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/pdf/go-wayland/client"
)

// silentCompositor accepts connections on a unix socket and then never sends
// anything back. This is the situation that used to wedge the dock: a capture
// waiting on an event the compositor will never deliver.
func silentCompositor(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), `wayland-test`)

	ln, err := net.Listen(`unix`, path)
	if err != nil {
		t.Fatalf(`listen on %s: %v`, path, err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})

	return path
}

func testApp(t *testing.T) *App {
	t.Helper()

	display, err := client.Connect(silentCompositor(t))
	if err != nil {
		t.Fatalf(`connect: %v`, err)
	}

	app := &App{display: display, log: hclog.NewNullLogger()}
	t.Cleanup(func() { app.kill() })

	return app
}

// Regression test for the dock freezing permanently on a preview hover.
//
// Context.Dispatch blocks in a read with no deadline. When it was used as the
// default branch of a select, the select's timeout case was never reached
// again and this call never returned, taking the GTK main loop down with it.
func TestDispatchUntilReturnsWhenCompositorGoesSilent(t *testing.T) {
	app := testApp(t)

	returned := make(chan error, 1)
	go func() {
		returned <- app.dispatchUntil(newWaiter(), 200*time.Millisecond)
	}()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal(`expected a timeout error, got nil`)
		}
	case <-time.After(10 * time.Second):
		t.Fatal(`dispatchUntil never returned: the blocking read is not interruptible`)
	}

	if !app.dead.Load() {
		t.Error(`connection should be marked dead after a timeout`)
	}
}

// Once the connection has been dropped, later calls must fail fast rather than
// operate on a closed context.
func TestDispatchUntilFailsFastOnDeadConnection(t *testing.T) {
	app := testApp(t)
	app.kill()

	err := app.dispatchUntil(newWaiter(), time.Minute)
	if !errors.Is(err, ErrConnectionDead) {
		t.Fatalf(`err = %v, want %v`, err, ErrConnectionDead)
	}
}

// The timeout has to stay recognisable through the wrapping the preview layers
// add, because it is the one capture failure that is expected rather than a
// fault: a window that closes mid-capture is never answered.
func TestDispatchUntilTimeoutIsIdentifiable(t *testing.T) {
	app := testApp(t)

	err := app.dispatchUntil(newWaiter(), 50*time.Millisecond)
	if !errors.Is(fmt.Errorf(`failed to capture frame: %w`, err), ErrCompositorTimeout) {
		t.Fatalf(`err = %v, want it to wrap %v`, err, ErrCompositorTimeout)
	}
}

// Preview.FPS reaches here straight from the config file with no range check,
// and a zero used to divide by zero inside the ticker.
func TestStartStreamRejectsNonPositiveFPS(t *testing.T) {
	app := testApp(t)

	for _, fps := range []int{0, -1} {
		if _, err := app.StartStream(0, fps, 1); err == nil {
			t.Errorf(`StartStream(fps=%d) = nil, want an error`, fps)
		}
	}
}

// A dead connection never revives, so a live stream that keeps ticking against
// one produces nothing for the rest of its life while looking healthy.
func TestStartStreamEndsOnDeadConnection(t *testing.T) {
	app := testApp(t)
	app.kill()

	stream, err := app.StartStream(0, 100, 1)
	if err != nil {
		t.Fatalf(`StartStream: %v`, err)
	}

	select {
	case _, ok := <-stream.Frames:
		if ok {
			t.Fatal(`a dead connection cannot produce frames`)
		}
	case <-time.After(5 * time.Second):
		t.Fatal(`stream never ended: it is ticking against a closed socket`)
	}

	if !errors.Is(stream.Err(), ErrConnectionDead) {
		t.Fatalf(`stream.Err() = %v, want %v`, stream.Err(), ErrConnectionDead)
	}
}

func TestDispatchUntilReturnsWaiterOutcome(t *testing.T) {
	sentinel := errors.New(`capture failed`)

	tests := map[string]error{
		`success`: nil,
		`failure`: sentinel,
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			app := testApp(t)

			w := newWaiter()
			w.settle(want)

			if got := app.dispatchUntil(w, time.Minute); !errors.Is(got, want) {
				t.Fatalf(`dispatchUntil = %v, want %v`, got, want)
			}
		})
	}
}

func TestWaiterSettlesOnce(t *testing.T) {
	first := errors.New(`first`)

	w := newWaiter()
	w.settle(first)
	w.settle(errors.New(`second`))

	select {
	case <-w.done():
	default:
		t.Fatal(`waiter should be settled`)
	}

	if !errors.Is(w.err, first) {
		t.Fatalf(`w.err = %v, want %v`, w.err, first)
	}
}

// Cleanup after a dropped connection fails by construction: the socket is gone,
// so every teardown request reports a write error that says nothing about the
// capture. Logging those at error made a bounded timeout look like a fault.
func TestTeardownDemotesErrorsAfterConnectionDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		dead bool
		want string
	}{
		{name: `live connection`, dead: false, want: `[ERROR]`},
		{name: `dropped connection`, dead: true, want: `[TRACE]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			app := &App{log: hclog.New(&hclog.LoggerOptions{
				Output: &logged,
				Level:  hclog.Trace,
			})}
			app.dead.Store(tc.dead)

			app.teardown(`failed destroying buffer`, errors.New(`use of closed network connection`))

			if got := logged.String(); !strings.Contains(got, tc.want) {
				t.Fatalf(`logged %q, want it to contain %s`, got, tc.want)
			}
		})
	}
}

func TestTeardownIgnoresSuccess(t *testing.T) {
	var logged bytes.Buffer
	app := &App{log: hclog.New(&hclog.LoggerOptions{Output: &logged, Level: hclog.Trace})}

	app.teardown(`failed destroying buffer`, nil)

	if logged.Len() != 0 {
		t.Fatalf(`logged %q, want nothing`, logged.String())
	}
}
