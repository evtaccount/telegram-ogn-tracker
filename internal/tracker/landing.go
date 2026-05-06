package tracker

import (
	"math"
	"time"

	"ogn/parser"
)

const (
	// landingSpeedThreshold — below this ground speed (km/h) the pilot is
	// likely on the ground. Walking speed is the upper bound; anything faster
	// can be slow flight or strong wind drift.
	landingSpeedThreshold = 5.0

	// landingClimbThreshold — vertical speed (m/s) magnitude that disqualifies
	// "on ground". Even weak thermals exceed 0.3 m/s.
	landingClimbThreshold = 0.3

	// landingConfirmDuration — how long both speed and climb must stay near
	// zero before we confirm the landing.
	landingConfirmDuration = 90 * time.Second
)

// landingEvent captures everything sendLandingAlert needs, so the alert can be
// emitted outside the mutex.
type landingEvent struct {
	id   string
	name string
	lat  float64
	lon  float64
	alt  float64
	time time.Time
	tz   *time.Location
}

// updateLandingState advances the pilot's "on the ground" timer based on the
// fresh position message and reports whether the pilot just transitioned from
// flying to landed.
//
// The function mutates info.LowSpeedSince, info.Status, and info.LandingTime.
// It is pure with respect to anything else, which makes the landing rules
// straightforward to unit-test.
//
// Caller must hold whatever mutex protects info.
func updateLandingState(info *TrackInfo, msg *parser.PositionMessage, now time.Time) bool {
	if info == nil || msg == nil || info.Status != StatusFlying {
		return false
	}

	onGround := msg.GroundSpeed < landingSpeedThreshold &&
		math.Abs(msg.ClimbRate) < landingClimbThreshold

	if !onGround {
		info.LowSpeedSince = time.Time{}
		return false
	}

	if info.LowSpeedSince.IsZero() {
		info.LowSpeedSince = now
		return false
	}

	if now.Sub(info.LowSpeedSince) <= landingConfirmDuration {
		return false
	}

	info.Status = StatusLanded
	info.LandingTime = now
	return true
}
