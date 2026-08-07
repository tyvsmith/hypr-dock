package pvwidget

import (
	"testing"
	"time"

	"hypr-dock/internal/pkg/conf"
	"hypr-dock/internal/settings"
)

// testWidget builds only the readiness tally. The GTK side is untouched: these
// are the bookkeeping paths that decide whether a preview ever opens.
func testWidget(t *testing.T, windows int) (*Widget, *int) {
	t.Helper()

	opened := 0
	return &Widget{
		expectedCount: windows,
		settings: &settings.Settings{
			Config: &conf.Config{General: conf.General{ContextPos: 5}},
		},
		onReady: func(w, h int) { opened++ },
	}, &opened
}

// ready records a window reporting its size, as the capture callback does -
// including releasing the lock before the callback runs.
func (w *Widget) ready(padding int) {
	w.mutex.Lock()
	w.readyCount++
	notify := w.notifyIfReadyLocked(padding)
	w.mutex.Unlock()

	if notify != nil {
		notify()
	}
}

func TestPreviewOpensOnceEveryWindowReports(t *testing.T) {
	w, opened := testWidget(t, 2)

	w.ready(4)
	if *opened != 0 {
		t.Fatal(`opened before every window reported`)
	}

	w.ready(4)
	if *opened != 1 {
		t.Fatalf(`opened %d times, want 1`, *opened)
	}
}

// A window that never answers used to leave readyCount short of expectedCount
// forever, so the preview silently never opened for any of the others.
func TestPreviewOpensAfterAWindowIsDropped(t *testing.T) {
	w, opened := testWidget(t, 2)

	w.ready(4)
	w.dropWindow(4)

	if *opened != 1 {
		t.Fatalf(`opened %d times, want 1 after the unresponsive window was dropped`, *opened)
	}
}

func TestPreviewReportsEmptyWhenEveryWindowIsDropped(t *testing.T) {
	w, opened := testWidget(t, 2)

	empty := 0
	w.onEmpty = func() { empty++ }

	w.dropWindow(4)
	w.dropWindow(4)

	if empty != 1 {
		t.Fatalf(`onEmpty fired %d times, want 1`, empty)
	}
	if *opened != 0 {
		t.Fatalf(`opened %d times, want 0 with no windows left`, *opened)
	}
}

// Captures resolve after the widget is built - a timeout takes seconds - so
// they can land once the popup has destroyed this box and moved on to another
// item. Firing onReady from there reopens the popup around destroyed content
// and segfaults inside gtk_widget_show_all.
func TestDestroyedWidgetIgnoresLateCaptureFailure(t *testing.T) {
	w, opened := testWidget(t, 2)

	w.ready(4)
	w.invalidate()
	w.dropWindow(4)

	if *opened != 0 {
		t.Fatalf(`opened %d times, want 0 for a destroyed widget`, *opened)
	}
}

func TestDestroyedWidgetIgnoresLateCaptureSuccess(t *testing.T) {
	w, opened := testWidget(t, 1)

	w.invalidate()
	w.ready(4)

	if *opened != 0 {
		t.Fatalf(`opened %d times, want 0 for a destroyed widget`, *opened)
	}
}

// onEmpty hides the preview, so a destroyed widget reporting empty would close
// the one now on screen.
func TestDestroyedWidgetDoesNotReportEmpty(t *testing.T) {
	w, _ := testWidget(t, 1)

	empty := 0
	w.onEmpty = func() { empty++ }

	w.invalidate()
	w.dropWindow(4)

	if empty != 0 {
		t.Fatalf(`onEmpty fired %d times, want 0 for a destroyed widget`, empty)
	}
}

// runsWithoutDeadlock reports whether call returns, rather than parking on a
// lock it already holds.
func runsWithoutDeadlock(t *testing.T, call func()) bool {
	t.Helper()

	done := make(chan struct{})
	go func() {
		call()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// The popup destroys the widget it replaces, and once this widget is the
// content that is this widget - so onReady re-enters invalidate, which takes
// w.mutex. Holding the lock across the callback deadlocked the GTK main
// thread and froze the whole dock.
func TestReadyCallbackRunsWithoutTheLockHeld(t *testing.T) {
	w, _ := testWidget(t, 1)
	w.onReady = func(int, int) { w.invalidate() }

	if !runsWithoutDeadlock(t, func() { w.ready(4) }) {
		t.Fatal(`deadlock: onReady ran while holding w.mutex`)
	}
}

// onEmpty closes the popup, which destroys this widget for the same reason.
func TestEmptyCallbackRunsWithoutTheLockHeld(t *testing.T) {
	w, _ := testWidget(t, 1)
	w.onEmpty = func() { w.invalidate() }

	if !runsWithoutDeadlock(t, func() { w.dropWindow(4) }) {
		t.Fatal(`deadlock: onEmpty ran while holding w.mutex`)
	}
}

// onReady resizes and positions the popup, so a second call would move a
// preview the user is already looking at.
func TestPreviewOpensOnlyOnce(t *testing.T) {
	w, opened := testWidget(t, 1)

	w.ready(4)
	w.ready(4)
	w.dropWindow(4)

	if *opened != 1 {
		t.Fatalf(`opened %d times, want 1`, *opened)
	}
}
