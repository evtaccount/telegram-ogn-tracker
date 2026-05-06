package tracker

import (
	"errors"
	"math"
	"strings"
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

func TestParseAllowedChats(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[int64]bool
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"single id", "-100123", map[int64]bool{-100123: true}},
		{"multiple ids", "-100123,456,-789", map[int64]bool{-100123: true, 456: true, -789: true}},
		{"with spaces", " -100 , 200 ", map[int64]bool{-100: true, 200: true}},
		{"trailing comma", "100,", map[int64]bool{100: true}},
		{"all invalid", "abc,xyz", nil},
		{"mixed valid/invalid", "100,abc,200", map[int64]bool{100: true, 200: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseAllowedChats(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("len(got)=%d len(want)=%d, got=%v want=%v", len(got), len(c.want), got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("got[%d]=%v want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestIsAllowedChat(t *testing.T) {
	t.Run("nil allow-list permits all", func(t *testing.T) {
		tr := &Tracker{}
		if !tr.isAllowedChat(123) {
			t.Error("nil allow-list should permit any chat")
		}
		if !tr.isAllowedChat(-100456) {
			t.Error("nil allow-list should permit negative chat IDs too")
		}
	})
	t.Run("set allow-list restricts", func(t *testing.T) {
		tr := &Tracker{allowedChats: map[int64]bool{-100123: true, 456: true}}
		if !tr.isAllowedChat(-100123) {
			t.Error("listed chat should be allowed")
		}
		if !tr.isAllowedChat(456) {
			t.Error("listed chat should be allowed")
		}
		if tr.isAllowedChat(789) {
			t.Error("non-listed chat should be denied")
		}
		if tr.isAllowedChat(0) {
			t.Error("zero should be denied when allow-list is set")
		}
	})
}

func TestSaveStateAsync(t *testing.T) {
	t.Run("saveState requests an async write", func(t *testing.T) {
		tr := &Tracker{
			users:    make(map[int64]*UserInfo),
			saveCh:   make(chan []byte, 1),
			saveDone: make(chan struct{}),
		}
		tr.users[1] = &UserInfo{UserID: 1, OGNID: "ABC"}

		tr.mu.Lock()
		tr.saveState()
		tr.mu.Unlock()

		select {
		case data := <-tr.saveCh:
			if len(data) == 0 {
				t.Fatal("expected a non-empty snapshot")
			}
		default:
			t.Fatal("expected a snapshot in the channel")
		}
	})

	t.Run("saveState replaces a stale pending snapshot", func(t *testing.T) {
		tr := &Tracker{
			users:    make(map[int64]*UserInfo),
			saveCh:   make(chan []byte, 1),
			saveDone: make(chan struct{}),
		}
		tr.users[1] = &UserInfo{UserID: 1, OGNID: "OLD"}

		tr.mu.Lock()
		tr.saveState()
		tr.mu.Unlock()

		tr.users[1].OGNID = "NEW"
		tr.mu.Lock()
		tr.saveState()
		tr.mu.Unlock()

		data := <-tr.saveCh
		if len(data) == 0 || !strings.Contains(string(data), "NEW") || strings.Contains(string(data), "OLD") {
			t.Fatalf("expected only the latest snapshot, got: %s", data)
		}
	})

	t.Run("saveState is a no-op after Shutdown flag is set", func(t *testing.T) {
		tr := &Tracker{
			users:        make(map[int64]*UserInfo),
			saveCh:       make(chan []byte, 1),
			saveDone:     make(chan struct{}),
			shuttingDown: true,
		}
		tr.users[1] = &UserInfo{UserID: 1, OGNID: "ABC"}

		tr.mu.Lock()
		tr.saveState()
		tr.mu.Unlock()

		select {
		case <-tr.saveCh:
			t.Fatal("expected no snapshot once shuttingDown is set")
		default:
		}
	})
}

func TestIsValidShortID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"FE0E4A", true},
		{"123ABC", true},
		{"ABCDEF", true},
		{"000000", true},
		{"FFFFFF", true},
		{"", false},
		{"FE0E4", false},   // 5 chars
		{"FE0E4AB", false}, // 7 chars
		{"GE0E4A", false},  // G is not hex
		{"FE0E4G", false},  // G is not hex
		{"fe0e4a", false},  // lowercase — caller should run shortID first
		{"AB CDE", false},  // space
		{"AB-CDE", false},  // dash
		{"АВ123Б", false},  // cyrillic
	}
	for _, c := range cases {
		got := isValidShortID(c.in)
		if got != c.want {
			t.Errorf("isValidShortID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsMessageNotModified(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		if isMessageNotModified(nil) {
			t.Error("nil should return false")
		}
	})
	t.Run("matches substring", func(t *testing.T) {
		err := errors.New("Bad Request: message is not modified: specified content...")
		if !isMessageNotModified(err) {
			t.Error("expected substring match")
		}
	})
	t.Run("unrelated error", func(t *testing.T) {
		err := errors.New("network is unreachable")
		if isMessageNotModified(err) {
			t.Error("unrelated error should not match")
		}
	})
}

func TestNextReconnectDelay(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{30 * time.Second, 60 * time.Second},
		{60 * time.Second, 60 * time.Second},  // already at cap
		{120 * time.Second, 60 * time.Second}, // overshoots cap
	}
	for _, c := range cases {
		got := nextReconnectDelay(c.in)
		if got != c.want {
			t.Errorf("nextReconnectDelay(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStaleLowSpeedReset(t *testing.T) {
	// Verify the boundary used by loadState: timestamps within the staleness
	// window are preserved, older ones are reset on load.
	if staleLowSpeedWindow <= 0 {
		t.Fatal("staleLowSpeedWindow must be positive")
	}

	now := time.Now()
	cases := []struct {
		name      string
		since     time.Time
		wantReset bool
	}{
		{"zero stays zero", time.Time{}, false},
		{"recent preserved", now.Add(-30 * time.Second), false},
		{"just inside window preserved", now.Add(-staleLowSpeedWindow + time.Second), false},
		{"just outside window resets", now.Add(-staleLowSpeedWindow - time.Second), true},
		{"hours-old resets", now.Add(-2 * time.Hour), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			low := c.since
			if !low.IsZero() && time.Since(low) > staleLowSpeedWindow {
				low = time.Time{}
			}
			gotReset := !c.since.IsZero() && low.IsZero()
			if gotReset != c.wantReset {
				t.Errorf("reset=%v want=%v (since=%v)", gotReset, c.wantReset, c.since)
			}
		})
	}
}
