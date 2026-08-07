package wl

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/pdf/go-wayland/client"
	"golang.org/x/sys/unix"
)

const (
	// The compositor answers CaptureToplevel with its supported formats
	// straight away, so this only has to absorb scheduling jitter.
	bufferDoneTimeout = 500 * time.Millisecond
	// ready follows an actual copy of the window contents into shared
	// memory, which can be slow on a busy GPU.
	frameReadyTimeout = 2 * time.Second
	roundTripTimeout  = 2 * time.Second
)

var (
	// ErrCompositorTimeout reports an exchange the compositor never answered.
	// Hyprland sends neither ready nor failed for a toplevel that is destroyed
	// mid-export, so this is the expected outcome of racing a closing window
	// rather than a fault.
	ErrCompositorTimeout = errors.New(`compositor did not respond`)

	// ErrConnectionDead reports a connection that has been torn down. It is
	// terminal: an App never reconnects, so the owner has to build a new one.
	ErrConnectionDead = errors.New(`wayland connection is no longer usable`)
)

type App struct {
	display  *client.Display
	registry *client.Registry
	shm      *client.Shm
	tl       *HyprlandToplevelExportManagerV1
	log      hclog.Logger

	// dead is set once the connection has been torn down, so later calls fail
	// fast instead of using a closed context. It is atomic because kill runs
	// both from the caller awaiting a timeout and from handleDisplayError,
	// which the dispatch goroutine invokes from inside Dispatch.
	dead atomic.Bool
}

// waiter is a one-shot signal carrying the outcome of a Wayland event handler
// back to whoever is pumping the event loop. Handlers run synchronously inside
// Dispatch, so a settle is always visible to the dispatch loop's next
// iteration.
type waiter struct {
	once sync.Once
	ch   chan struct{}
	err  error
}

func newWaiter() *waiter {
	return &waiter{ch: make(chan struct{})}
}

func (w *waiter) settle(err error) {
	w.once.Do(func() {
		w.err = err
		close(w.ch)
	})
}

func (w *waiter) done() <-chan struct{} {
	return w.ch
}

type shmPool struct {
	*client.ShmPool
	fd   int
	data []byte
}

type FrameStream struct {
	Frames chan *image.NRGBA
	stop   chan struct{}
	err    error
}

func NewApp(log hclog.Logger) (*App, error) {
	display, err := client.Connect(``)
	if err != nil {
		return nil, err
	}

	registry, err := display.GetRegistry()
	if err != nil {
		return nil, err
	}

	app := &App{
		display:  display,
		registry: registry,
		log:      log.Named(`wl`),
	}
	display.SetErrorHandler(app.handleDisplayError)
	registry.SetGlobalHandler(app.handleRegistryGlobal)

	// init registry
	if err := app.roundTrip(); err != nil {
		return nil, err
	}

	// get events
	if err := app.roundTrip(); err != nil {
		return nil, err
	}

	return app, nil
}

func (a *App) StartStream(handle uint64, fps int, bufferSize int) (*FrameStream, error) {
	// Preview.FPS is unvalidated config, and a zero would divide by zero
	// below rather than fail here.
	if fps <= 0 {
		return nil, fmt.Errorf(`fps must be positive, got %d`, fps)
	}

	stream := &FrameStream{
		Frames: make(chan *image.NRGBA, bufferSize),
		stop:   make(chan struct{}),
	}

	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(fps))
		defer ticker.Stop()

		for {
			select {
			case <-stream.stop:
				close(stream.Frames)
				return
			case <-ticker.C:
				frame, err := a.CaptureFrame(handle)
				if err != nil {
					// A dead connection never revives, so ticking on would
					// spin against a closed socket forever. End the stream
					// instead and let the owner build a new App.
					if a.dead.Load() {
						stream.err = err
						close(stream.Frames)
						return
					}

					a.log.Trace(`capture error`, `err`, err)
					continue
				}

				select {
				case stream.Frames <- frame:
				default:
					a.log.Trace("Buffer full")
				}
			}
		}
	}()

	return stream, nil
}

// Err reports why the stream ended. It is only meaningful once Frames has been
// closed, and is nil for a stream ended by Stop.
func (s *FrameStream) Err() error {
	return s.err
}

func (s *FrameStream) Stop() {
	close(s.stop)
}

func (a *App) CaptureFrame(handle uint64) (*image.NRGBA, error) {
	if a.dead.Load() {
		return nil, ErrConnectionDead
	}
	if a.tl == nil {
		return nil, errors.New(`toplevel export manager not available`)
	}

	frame, err := a.tl.CaptureToplevel(0, uint32(handle))
	if err != nil {
		return nil, err
	}
	defer func() { a.teardown(`failed destroying frame`, frame.Destroy()) }()

	formats := make([]HyprlandToplevelExportFrameV1BufferEvent, 0)
	bufferDone := newWaiter()
	ready := newWaiter()

	frame.SetBufferHandler(func(evt HyprlandToplevelExportFrameV1BufferEvent) {
		formats = append(formats, evt)
	})
	frame.SetBufferDoneHandler(func(evt HyprlandToplevelExportFrameV1BufferDoneEvent) {
		bufferDone.settle(nil)
	})
	frame.SetReadyHandler(func(evt HyprlandToplevelExportFrameV1ReadyEvent) {
		ready.settle(nil)
	})
	frame.SetFailedHandler(func(evt HyprlandToplevelExportFrameV1FailedEvent) {
		// The compositor can fail the capture at either stage, so settle
		// both; settle is idempotent and only the pending one is awaited.
		err := errors.New(`compositor reported frame capture failure`)
		bufferDone.settle(err)
		ready.settle(err)
	})

	if err := a.dispatchUntil(bufferDone, bufferDoneTimeout); err != nil {
		return nil, err
	}

	if len(formats) == 0 {
		return nil, errors.New(`no buffer formats`)
	}

	a.log.Debug("Available buffer formats:", "count", len(formats))
	for i, format := range formats {
		a.log.Debug(fmt.Sprintf("Format %d:", i),
			"width", format.Width,
			"height", format.Height,
			"stride", format.Stride,
			"format", format.Format,
			"shm_format_name", client.ShmFormat(format.Format).String(),
		)
	}

	var selected *HyprlandToplevelExportFrameV1BufferEvent
OUTER:
	for _, format := range formats {
		switch client.ShmFormat(format.Format) {
		case client.ShmFormatArgb8888:
			selected = &format
			break OUTER
		case client.ShmFormatXrgb8888:
			selected = &format
			break OUTER
		}
	}

	if selected == nil {
		return nil, errors.New(`no suitable buffer format`)
	}

	pool, err := a.createShmPool(int32(selected.Height * selected.Stride))
	if err != nil {
		return nil, err
	}
	defer func() { a.teardown(`failed closing SHM pool`, pool.Close()) }()

	buf, err := pool.CreateBuffer(0, int32(selected.Width), int32(selected.Height), int32(selected.Stride), selected.Format)
	if err != nil {
		return nil, err
	}
	defer func() { a.teardown(`failed destroying buffer`, buf.Destroy()) }()

	if err := frame.Copy(buf, 1); err != nil {
		return nil, err
	}

	// The compositor sends ready asynchronously once it has copied the frame
	// data, so events must keep being dispatched until it arrives.
	if err := a.dispatchUntil(ready, frameReadyTimeout); err != nil {
		return nil, err
	}

	data := pool.Data()
	img := image.NewNRGBA(image.Rect(0, 0, int(selected.Width), int(selected.Height)))
	if len(img.Pix) < int(selected.Height)*int(selected.Stride) {
		return nil, errors.New(`image buffer too small`)
	}
	for y := range int(selected.Height) {
		for x := range int(selected.Width) {
			pix := data[y*int(selected.Stride)+(x*4) : y*int(selected.Stride)+(x*4)+4]
			col := color.NRGBA{}
			switch client.ShmFormat(selected.Format) {
			case client.ShmFormatArgb8888:
				col.A = pix[3]
				col.R = pix[2]
				col.G = pix[1]
				col.B = pix[0]
			case client.ShmFormatXrgb8888:
				col.A = 0xff
				col.R = pix[2]
				col.G = pix[1]
				col.B = pix[0]
			}
			img.SetNRGBA(x, y, col)
		}
	}

	return img, nil
}

func (a *App) Close() error {
	// Nothing to send once the socket is gone.
	if a.dead.Load() {
		return nil
	}
	if a.tl != nil {
		if err := a.tl.Destroy(); err != nil {
			return err
		}
	}
	if a.shm != nil {
		if err := a.shm.Release(); err != nil {
			return nil
		}
	}
	if a.registry != nil {
		if err := a.registry.Destroy(); err != nil {
			return err
		}
	}
	if a.display != nil {
		if err := a.display.Destroy(); err != nil {
			return err
		}
	}
	return nil
}

func (p *shmPool) Data() []byte {
	return p.data
}

// Close releases the mapping, the compositor-side pool and the memfd. Every
// step runs even when an earlier one fails, so a Destroy that cannot reach the
// compositor still leaves no descriptor behind.
func (p *shmPool) Close() error {
	return errors.Join(
		unix.Munmap(p.data),
		p.Destroy(),
		unix.Close(p.fd),
	)
}

// teardown reports a cleanup request that failed. On a dropped connection every
// request fails with the same write error, which describes the teardown rather
// than the capture, so those are logged at trace.
func (a *App) teardown(msg string, err error) {
	if err == nil {
		return
	}
	if a.dead.Load() {
		a.log.Trace(msg, `err`, err)
		return
	}
	a.log.Error(msg, `err`, err)
}

func (a *App) handleDisplayError(evt client.DisplayErrorEvent) {
	// This runs inside Dispatch, on the goroutine currently reading the
	// connection. Reconnecting here would close and replace the display out
	// from under that reader, so the connection is only marked unusable;
	// callers create a fresh App per capture.
	a.log.Error(`wayland display error, connection is now unusable`, `error`, evt)
	a.kill()
}

func (a *App) handleShmFormat(evt client.ShmFormatEvent) {
	a.log.Trace(`reported available SHM format`, `format`, client.ShmFormat(evt.Format))
}

func (a *App) handleRegistryGlobal(evt client.RegistryGlobalEvent) {
	a.log.Trace(`global object`, `name`, evt.Name, `interface`, evt.Interface, `version`, evt.Version)

	switch evt.Interface {
	case `wl_shm`:
		shm := client.NewShm(a.display.Context())
		if err := a.registry.Bind(evt.Name, evt.Interface, evt.Version, shm); err != nil {
			a.log.Error(`failed binding SHM`, `err`, err)
			return
		}
		shm.SetFormatHandler(a.handleShmFormat)
		a.shm = shm
	case `hyprland_toplevel_export_manager_v1`:
		tl := NewHyprlandToplevelExportManagerV1(a.display.Context())

		// Caps at 2 since that is what the XML/generated code supports
		bindVer := evt.Version
		if bindVer > 2 {
			bindVer = 2
		}

		if err := a.registry.Bind(evt.Name, evt.Interface, bindVer, tl); err != nil {
			a.log.Error(`failed binding toplevel export manager`, `err`, err)
			return
		}
		a.tl = tl
	}
}

func (a *App) createShmPool(size int32) (*shmPool, error) {
	fd, err := unix.MemfdCreate("hypr-dock-shm", 0)
	if err != nil {
		return nil, fmt.Errorf(`failed creating memfd: %w`, err)
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		return nil, fmt.Errorf(`failed truncating memfd: %w`, err)
	}

	data, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf(`failed mmapping memfd: %w`, err)
	}

	pool, err := a.shm.CreatePool(fd, int32(size))
	if err != nil {
		return nil, fmt.Errorf(`failed creating SHM pool: %w`, err)
	}

	return &shmPool{
		ShmPool: pool,
		fd:      fd,
		data:    data,
	}, nil
}

// dispatchUntil pumps Wayland events until w settles or timeout elapses.
//
// Context.Dispatch blocks in a read with no deadline, so the loop has to run on
// its own goroutine for the timeout to be observable at all. Callers must not
// overlap two dispatchUntil calls on one App: go-wayland reads a message header
// and body in two steps without a lock, so a second reader would interleave
// with the first and desynchronise the framing.
func (a *App) dispatchUntil(w *waiter, timeout time.Duration) error {
	if a.dead.Load() {
		return ErrConnectionDead
	}

	dispatchErr := make(chan error, 1)
	go func() {
		for {
			// Handlers settle w from inside Dispatch, so checking here
			// before blocking again terminates the loop as soon as the
			// awaited event lands.
			select {
			case <-w.done():
				return
			default:
			}

			if err := a.display.Context().Dispatch(); err != nil {
				dispatchErr <- err
				return
			}
		}
	}()

	select {
	case <-w.done():
		return w.err

	case err := <-dispatchErr:
		return fmt.Errorf(`wayland dispatch failed: %w`, err)

	case <-time.After(timeout):
		// The reader is parked in a blocking read that no deadline can
		// interrupt; closing the connection is the only way to release it.
		// A capture that has already wedged cannot be resumed anyway.
		a.log.Warn(`compositor did not respond, dropping connection`, `timeout`, timeout)
		a.kill()
		return fmt.Errorf(`%w after %s`, ErrCompositorTimeout, timeout)
	}
}

// kill tears down the connection to release a reader blocked in Dispatch. It is
// safe to call from either goroutine and only the first call closes.
func (a *App) kill() {
	if a.dead.Swap(true) {
		return
	}
	if err := a.display.Context().Close(); err != nil {
		a.log.Trace(`failed closing wayland context`, `err`, err)
	}
}

func (a *App) roundTrip() error {
	cb, err := a.display.Sync()
	if err != nil {
		return err
	}
	defer cb.Destroy()

	w := newWaiter()
	cb.SetDoneHandler(func(_ client.CallbackDoneEvent) {
		w.settle(nil)
	})

	return a.dispatchUntil(w, roundTripTimeout)
}
