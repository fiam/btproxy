package esphome

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3*time.Minute + 12*time.Second, "3m 12s"},
		{2*time.Hour + 3*time.Minute + 12*time.Second, "2h 3m 12s"},
		{26*time.Hour + 3*time.Minute, "1d 2h 3m 0s"},
		{9*24*time.Hour + 8*time.Hour + 2*time.Minute + 11*time.Second, "9d 8h 2m 11s"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, FormatUptime(c.d), "uptime for %v", c.d)
	}
}
