package main

import (
	"math"
	"testing"
)

func TestLastSpeedX(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"empty", "", 0},
		{"none", "frame=1 fps=24 q=-0.0", 0},
		{"single", "speed=4.47x", 4.47},
		{"space-padded", "speed= 5.3x", 5.3},
		{"multiple progress on one line via CR", "speed=0.0873x    speed= 5.3x    speed=4.92x", 4.92},
		{"final integer-ish", "speed=24x", 24},
		{"trailing junk", "speed=2.50x [out#0/null]", 2.50},
		{"with full progress", "frame=  72 fps=0.0 q=-0.0 Lsize=N/A time=00:00:02.91 bitrate=N/A speed=4.92x", 4.92},
	}
	for _, c := range cases {
		got := lastSpeedX(c.in)
		if math.Abs(got-c.want) > 0.001 {
			t.Errorf("%s: got %.4f want %.4f", c.name, got, c.want)
		}
	}
}
