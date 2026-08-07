package pvwidget

import (
	"fmt"
	"hypr-dock/internal/hysc"
	"hypr-dock/internal/item"
	"hypr-dock/internal/pkg/utils"
	"hypr-dock/internal/settings"
	"hypr-dock/pkg/ipc"

	"sync"

	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/glib"
	"github.com/gotk3/gotk3/gtk"
	"github.com/gotk3/gotk3/pango"
	"github.com/hashicorp/go-hclog"
)

type Widget struct {
	readyCount    int
	expectedCount int
	totalWidth    int
	commonHeight  int
	notified      bool
	destroyed     bool
	mutex         sync.Mutex

	settings *settings.Settings
	item     *item.Item

	onReady  func(w, h int)
	onResize func(w, h int)
	onClick  func(*ipc.Client)
	onEmpty  func()

	log hclog.Logger

	*gtk.Box
}

func New(item *item.Item, settings *settings.Settings, log hclog.Logger) (*Widget, error) {
	wrapper, err := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, settings.ContextPos)
	if err != nil {
		return nil, err
	}
	wrapper.SetName("pv-wrap")

	widget := &Widget{
		Box:      wrapper,
		settings: settings,
		item:     item,
		onReady:  func(w, h int) { log.Trace("PV Widget ready", "width", w, "height", h) },
		onResize: func(w, h int) { log.Trace("PV Widget resize", "width", w, "height", h) },

		log: log,
	}

	// A capture resolves after the widget is built, so it can land once this
	// box has been destroyed - the popup destroys its content on every open,
	// and hovering another item opens a new one. Driving a callback from
	// there would use a dead GTK object, so late arrivals are dropped.
	wrapper.Connect("destroy", widget.invalidate)

	log.Debug("Creating preview",
		"class_name", item.ClassName,
		"window_count", len(item.Windows))

	for addr, win := range item.Windows {
		log.Debug("Window details",
			"address", addr,
			"title", win.Title)
	}

	successCount := 0
	for _, window := range item.Windows {
		err := widget.createWindowWidget(window)
		if err != nil {
			log.Error("Unable to create window widget", "error", err)
			continue
		}
		successCount++
	}
	widget.expectedCount = successCount

	if successCount == 0 {
		return nil, fmt.Errorf("failed to create any window widgets")
	}

	return widget, nil
}

func (w *Widget) createWindowWidget(window *ipc.Client) error {
	padding := w.settings.PreviewStyle.Padding

	windowBox, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	if err != nil {
		return err
	}
	windowBox.SetName("pv-item")

	eventBox, err := gtk.EventBoxNew()
	if err != nil {
		return err
	}
	eventBox.SetName("pv-event-box")

	windowBoxContent, err := gtk.BoxNew(gtk.ORIENTATION_VERTICAL, 0)
	if err != nil {
		return err
	}
	windowBoxContent.SetMarginBottom(padding)
	windowBoxContent.SetMarginEnd(padding)
	windowBoxContent.SetMarginStart(padding)
	windowBoxContent.SetMarginTop(padding / 2)

	titleBox, err := gtk.BoxNew(gtk.ORIENTATION_HORIZONTAL, 5)
	if err != nil {
		return err
	}
	titleBox.SetMarginBottom(padding / 2)

	icon, err := utils.CreateImage(w.item.App.GetIcon(), 16)
	if err != nil {
		return err
	}

	label, err := gtk.LabelNew(window.Title)
	if err != nil {
		return err
	}
	label.SetEllipsize(pango.ELLIPSIZE_END)
	label.SetXAlign(0)
	label.SetHExpand(true)
	label.SetTooltipText(window.Title)

	iconName := utils.GetFirstAvailableImage([]string{
		"close",
		"close-symbolic",
		"window-close",
		"window-close-symbolic",
	})

	closeBtn, err := gtk.ButtonNewFromIconName(iconName, gtk.ICON_SIZE_SMALL_TOOLBAR)
	if err != nil {
		return err
	}
	closeBtn.SetName("close-btn")
	utils.AddStyle(closeBtn, "#close-btn {padding: 0;}")

	eventBox.Connect("button-press-event", func(eb *gtk.EventBox, e *gdk.Event) {
		go ipc.Hyprctl("dispatch focuswindow address:" + window.Address)

		if w.onClick != nil {
			w.onClick(window)
		}
	})

	context, err := windowBox.GetStyleContext()
	if err == nil {
		utils.SetAutoHover(eventBox.ToWidget(), context)
	}
	utils.SetCursorPointer(eventBox.ToWidget())

	var stream *hysc.Stream
	w.log.Debug("Attempting to create stream window", "window", window.Title, "address", window.Address)

	stream, err = hysc.StreamNew(window.Address, w.log)
	if err != nil {
		w.log.Error("Stream creation failed", "address", window.Address, "error", err)
		return err
	}

	w.log.Debug("Stream created successfully", "address", window.Address)

	stream.OnReady(func(s *hysc.Size) {
		if s == nil {
			return
		}

		closeBtn.Connect("button-press-event", func() {
			go ipc.Hyprctl("dispatch closewindow address:" + window.Address)
			if len(w.item.Windows) == 1 {
				w.onEmpty()
				return
			}

			w.mutex.Lock()
			w.totalWidth = w.totalWidth - s.W - padding*2 - w.settings.ContextPos
			width, height := w.totalWidth, w.commonHeight
			w.mutex.Unlock()

			w.onResize(width, height)

			windowBox.Destroy()
			w.ShowAll()
		})

		glib.IdleAdd(func() {
			w.mutex.Lock()

			w.totalWidth += s.W
			w.readyCount++
			w.commonHeight = s.H

			notify := w.notifyIfReadyLocked(padding)
			w.mutex.Unlock()

			if notify != nil {
				notify()
			}
		})
	})

	stream.SetHScale(w.settings.PreviewStyle.Size)
	stream.SetBorderRadius(w.settings.PreviewStyle.BorderRadius)

	if w.settings.Preview.Mode == "live" {
		if err := stream.Start(w.settings.Preview.FPS, w.settings.Preview.BufferSize); err != nil {
			return err
		}
	} else {
		// Capture off the GTK main thread. This function runs inside a
		// glib.IdleAdd callback, so a synchronous capture would block the
		// main loop, freezing the whole dock until it returned. The result
		// is applied via glib.IdleAdd from inside CaptureFrame.
		go func() {
			if err := stream.CaptureFrame(); err != nil {
				// Racing a window that closes mid-capture is expected and
				// self-correcting - it is dropped and the preview opens
				// without it. Anything else is a genuine failure.
				log := w.log.Error
				if hysc.IsTransient(err) {
					log = w.log.Warn
				}
				log("Frame capture failed",
					"address", window.Address, "error", err)

				glib.IdleAdd(func() { w.dropWindow(padding) })
			}
		}()
	}

	titleBox.Add(icon)
	titleBox.Add(label)
	titleBox.Add(closeBtn)

	windowBoxContent.Add(titleBox)
	windowBoxContent.Add(stream)

	eventBox.Add(windowBoxContent)
	windowBox.Add(eventBox)
	w.Add(windowBox)

	return nil
}

// notifyIfReadyLocked returns the onReady call to make once every window still
// in the tally has reported its size, or nil when the preview is not ready.
//
// The caller must hold w.mutex and must make that call after releasing it.
// onReady opens the popup, which destroys the widget it is replacing - and
// once this widget is the content, that is this widget. Its destroy handler
// takes w.mutex, so running the callback under the lock deadlocks the GTK
// main thread.
func (w *Widget) notifyIfReadyLocked(padding int) func() {
	if w.destroyed || w.notified || w.expectedCount <= 0 || w.readyCount != w.expectedCount {
		return nil
	}
	w.notified = true

	w.totalWidth = w.totalWidth + w.settings.ContextPos*(w.expectedCount-1) + 2*padding*w.expectedCount
	w.commonHeight = w.commonHeight + 2*padding + 20

	width, height := w.totalWidth, w.commonHeight
	return func() { w.onReady(width, height) }
}

// dropWindow removes a window from the readiness tally after its capture
// failed, so that one unresponsive window cannot stop the preview from
// opening for the rest. Runs on the GTK main thread.
func (w *Widget) dropWindow(padding int) {
	w.mutex.Lock()
	if w.destroyed {
		w.mutex.Unlock()
		return
	}

	w.expectedCount--

	var notify func()
	if w.expectedCount <= 0 {
		notify = w.onEmpty
	} else {
		notify = w.notifyIfReadyLocked(padding)
	}
	w.mutex.Unlock()

	if notify != nil {
		notify()
	}
}

// invalidate marks the widget unusable once GTK has destroyed its box. A
// superseded preview must not drive the popup: onReady would reopen it around
// destroyed content, and onEmpty would close the one now on screen.
func (w *Widget) invalidate() {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	w.destroyed = true
}

func (w *Widget) OnResize(handler func(w, h int)) {
	w.onResize = handler
}

func (w *Widget) OnReady(handler func(w, h int)) {
	w.onReady = handler
}

func (w *Widget) OnClick(handler func(*ipc.Client)) {
	w.onClick = handler
}

func (w *Widget) OnEmpty(handler func()) {
	w.onEmpty = handler
}

func (w *Widget) GetClass() string {
	return w.item.ClassName
}
