package hysc

import (
	"errors"
	"fmt"
	"testing"

	"hypr-dock/pkg/wl"
)

// The predicate is read through the error CaptureFrameWithApp returns, which
// wraps what pkg/wl reported. It only works while that chain stays unbroken, so
// the wrapping is what this asserts.
func TestIsConnectionDeadSeesThroughCaptureWrapping(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		connectionEnd bool
	}{
		{name: `dead connection`, err: wl.ErrConnectionDead, connectionEnd: true},
		{name: `unsupported format`, err: errors.New(`no suitable buffer format`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The shape CaptureFrameWithApp returns.
			err := fmt.Errorf(`failed to capture frame: %w`, tc.err)

			if got := IsConnectionDead(err); got != tc.connectionEnd {
				t.Errorf(`IsConnectionDead = %v, want %v`, got, tc.connectionEnd)
			}
		})
	}
}

func TestPredicatesIgnoreNil(t *testing.T) {
	if IsConnectionDead(nil) {
		t.Error(`a successful capture is not a dead connection`)
	}
}
