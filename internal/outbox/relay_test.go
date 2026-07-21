package outbox

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	maximum := 30 * time.Second
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{5, 16 * time.Second},
		{10, maximum},
	} {
		if got := retryDelay(test.attempt, maximum); got != test.want {
			t.Errorf("attempt %d: got %s, want %s", test.attempt, got, test.want)
		}
	}
}
