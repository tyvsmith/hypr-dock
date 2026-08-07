package hysc

import (
	"errors"
	"fmt"
	"testing"

	"hypr-dock/pkg/wl"
)

// Both predicates are read through the error CaptureFrame returns, which wraps
// what pkg/wl reported. They only work while that chain stays unbroken, so the
// wrapping is what these assert.
func TestPredicatesSeeThroughCaptureWrapping(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		transient     bool
		connectionEnd bool
	}{
		{name: `timeout`, err: wl.ErrCompositorTimeout, transient: true},
		{name: `dead connection`, err: wl.ErrConnectionDead, connectionEnd: true},
		{name: `unsupported format`, err: errors.New(`no suitable buffer format`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The shape both CaptureFrame and CaptureFrameWithApp return.
			err := fmt.Errorf(`failed to capture frame: %w`, tc.err)

			if got := IsTransient(err); got != tc.transient {
				t.Errorf(`IsTransient = %v, want %v`, got, tc.transient)
			}
			if got := IsConnectionDead(err); got != tc.connectionEnd {
				t.Errorf(`IsConnectionDead = %v, want %v`, got, tc.connectionEnd)
			}
		})
	}
}

func TestPredicatesIgnoreNil(t *testing.T) {
	if IsTransient(nil) || IsConnectionDead(nil) {
		t.Error(`a successful capture is neither transient nor dead`)
	}
}
