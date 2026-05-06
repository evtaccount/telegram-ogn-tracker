package tracker

import (
	"math"
	"testing"
	"time"

	"ogn/ddb"
	"ogn/parser"
)

func TestShortID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"FLR123ABC", "123ABC"},
		{"123ABC", "123ABC"},
		{"  flr123abc  ", "123ABC"},
		{"abc", "ABC"},
		{"", ""},
		{"ICA3FE0E4A", "FE0E4A"},
		{"ognfe0e4a", "FE0E4A"},
	}
	for _, c := range cases {
		if got := shortID(c.in); got != c.want {
			t.Errorf("shortID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCommandArgs(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/add FLR123 Vasya", "FLR123 Vasya"},
		{"/add FLR123", "FLR123"},
		{"/add", ""},
		{"/add ", ""},
		{"/add   trim", "trim"},
		{"/help", ""},
		{"/start add_-100123", "add_-100123"},
	}
	for _, c := range cases {
		if got := commandArgs(c.in); got != c.want {
			t.Errorf("commandArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBearingName(t *testing.T) {
	cases := []struct {
		deg  float64
		want string
	}{
		{0, "N"},
		{45, "NE"},
		{90, "E"},
		{135, "SE"},
		{180, "S"},
		{225, "SW"},
		{270, "W"},
		{315, "NW"},
		{360, "N"},
		{-45, "NW"},
		{22, "N"},  // rounds down
		{23, "NE"}, // rounds up
	}
	for _, c := range cases {
		if got := bearingName(c.deg); got != c.want {
			t.Errorf("bearingName(%g) = %q, want %q", c.deg, got, c.want)
		}
	}
}

func TestDistanceAndBearing(t *testing.T) {
	// 1° of longitude at lat 50 ≈ 71.7 km on the WGS84 ellipsoid.
	dist, bearing := distanceAndBearing(50, 30, 50, 31)
	if dist < 70 || dist > 73 {
		t.Errorf("east 1° at lat 50: expected ~71km, got %.2f", dist)
	}
	if math.Abs(bearing-90) > 1 {
		t.Errorf("east bearing: expected ~90°, got %.2f", bearing)
	}

	// Same point — distance must be zero.
	if d, _ := distanceAndBearing(50, 30, 50, 30); d != 0 {
		t.Errorf("same point distance: expected 0, got %f", d)
	}
}

func TestNearestDriver(t *testing.T) {
	drivers := []*Coordinates{
		{Latitude: 50.0, Longitude: 30.0},
		{Latitude: 51.0, Longitude: 30.0}, // ~111 km north
	}

	dist, _, found := nearestDriver(50.001, 30.001, drivers)
	if !found {
		t.Fatal("expected found=true")
	}
	if dist > 1 {
		t.Errorf("nearest expected <1km, got %.3f", dist)
	}

	if _, _, found := nearestDriver(50, 30, nil); found {
		t.Error("empty drivers: expected found=false")
	}
	if _, _, found := nearestDriver(50, 30, []*Coordinates{}); found {
		t.Error("zero drivers: expected found=false")
	}
}

func TestUpdateLandingState(t *testing.T) {
	t0 := time.Now()
	flying := func() *TrackInfo { return &TrackInfo{Status: StatusFlying} }
	moving := &parser.PositionMessage{GroundSpeed: 30, ClimbRate: 0.0}
	stopped := &parser.PositionMessage{GroundSpeed: 1, ClimbRate: 0.1}
	thermalling := &parser.PositionMessage{GroundSpeed: 1, ClimbRate: 1.5}

	t.Run("flying with motion stays flying", func(t *testing.T) {
		info := flying()
		if updateLandingState(info, moving, t0) {
			t.Fatal("expected no transition")
		}
		if info.Status != StatusFlying {
			t.Errorf("status: got %v", info.Status)
		}
	})

	t.Run("low speed but climbing resets timer", func(t *testing.T) {
		info := flying()
		info.LowSpeedSince = t0.Add(-2 * time.Minute) // pretend we were stopped
		if updateLandingState(info, thermalling, t0) {
			t.Fatal("thermalling pilot must not be marked landed")
		}
		if !info.LowSpeedSince.IsZero() {
			t.Errorf("LowSpeedSince should reset, got %v", info.LowSpeedSince)
		}
	})

	t.Run("first stationary frame starts timer, no transition", func(t *testing.T) {
		info := flying()
		if updateLandingState(info, stopped, t0) {
			t.Fatal("first stationary frame must not transition yet")
		}
		if info.LowSpeedSince != t0 {
			t.Errorf("LowSpeedSince: got %v want %v", info.LowSpeedSince, t0)
		}
	})

	t.Run("stationary for less than confirm window — no transition", func(t *testing.T) {
		info := flying()
		info.LowSpeedSince = t0.Add(-30 * time.Second)
		if updateLandingState(info, stopped, t0) {
			t.Fatal("30s is below the 90s confirm window")
		}
	})

	t.Run("stationary past confirm window — landed", func(t *testing.T) {
		info := flying()
		info.LowSpeedSince = t0.Add(-2 * time.Minute)
		if !updateLandingState(info, stopped, t0) {
			t.Fatal("expected transition to landed")
		}
		if info.Status != StatusLanded {
			t.Errorf("status: got %v want StatusLanded", info.Status)
		}
		if info.LandingTime != t0 {
			t.Errorf("LandingTime: got %v want %v", info.LandingTime, t0)
		}
	})

	t.Run("already landed — no-op", func(t *testing.T) {
		info := &TrackInfo{Status: StatusLanded, LandingTime: t0.Add(-time.Minute)}
		if updateLandingState(info, stopped, t0) {
			t.Fatal("must not re-transition")
		}
	})

	t.Run("nil inputs are safe", func(t *testing.T) {
		if updateLandingState(nil, stopped, t0) {
			t.Error("nil info: expected false")
		}
		if updateLandingState(flying(), nil, t0) {
			t.Error("nil msg: expected false")
		}
	})
}

func TestFormatDDBInfo(t *testing.T) {
	if got := formatDDBInfo(nil, "ABC"); got != "" {
		t.Errorf("nil devices: expected empty, got %q", got)
	}
	if got := formatDDBInfo(map[string]ddb.Device{}, "ABC"); got != "" {
		t.Errorf("missing id: expected empty, got %q", got)
	}
	devices := map[string]ddb.Device{
		"FE0E4A": {AircraftModel: "ASG 29", Registration: "D-1234", CN: "AB"},
		"123ABC": {AircraftModel: "Discus"},
	}
	if got := formatDDBInfo(devices, "FE0E4A"); got != "ASG 29 | D-1234 | CN:AB" {
		t.Errorf("full record: got %q", got)
	}
	if got := formatDDBInfo(devices, "123ABC"); got != "Discus" {
		t.Errorf("model only: got %q", got)
	}
}
