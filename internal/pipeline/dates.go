package pipeline

import (
	"time"
)

// DateRules decides which recording dates to trust.
type DateRules struct {
	Zone *time.Location
	// ResetDates are the defaults a camcorder falls back to when its clock
	// battery dies (the VDR-M50 showed 1 January 2004). A recording on one
	// of these days is treated as undated.
	ResetDates []string // yyyy-mm-dd
	Now        time.Time
}

// Earliest plausible camcorder DVD recording.
var minPlausible = time.Date(1995, 1, 1, 0, 0, 0, 0, time.UTC)

// Sane reports whether a camera-local recording time is believable, and why
// not when it isn't.
func (r DateRules) Sane(t time.Time) (bool, string) {
	if t.IsZero() {
		return false, "no date on disc"
	}
	day := t.Format("2006-01-02")
	for _, d := range r.ResetDates {
		if d == day {
			return false, "date " + day + " is the camera's reset default (clock battery flat?)"
		}
	}
	if t.Before(minPlausible) {
		return false, "date " + day + " is before 1995"
	}
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	if t.After(now.Add(24 * time.Hour)) {
		return false, "date " + day + " is in the future"
	}
	return true, ""
}

// Localize places a camera-local wall-clock time (no zone) in the zone.
func (r DateRules) Localize(t time.Time) time.Time {
	z := r.Zone
	if z == nil {
		z = time.UTC
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, z)
}

// ClipDate is a resolved date for one clip.
type ClipDate struct {
	When   time.Time `json:"when,omitzero"`
	Source string    `json:"source"` // dvd-vr, disc-date, none
	Flag   string    `json:"flag,omitempty"`
}

// ResolveDates picks each clip's date: the disc's own per-recording time if
// sane, else the disc-level date with clips spaced one minute apart in
// recording order so they sort correctly in Photos, else unknown.
func ResolveDates(r DateRules, recTimes []time.Time, discDate time.Time) []ClipDate {
	out := make([]ClipDate, len(recTimes))
	for i, t := range recTimes {
		if ok, why := r.Sane(t); ok {
			out[i] = ClipDate{When: r.Localize(t), Source: "disc"}
			continue
		} else if !t.IsZero() {
			out[i].Flag = why
		}
		if !discDate.IsZero() {
			d := r.Localize(discDate)
			out[i].When = d.Add(time.Duration(i) * time.Minute)
			out[i].Source = "disc-date"
		} else {
			out[i].Source = "none"
		}
	}
	return out
}
