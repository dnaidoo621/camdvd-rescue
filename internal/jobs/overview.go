package jobs

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

// StallAfter is how long without progress before work counts as stuck.
var StallAfter = 10 * time.Minute

// liveEncode is what the engine knows about a running conversion.
type liveEncode struct {
	clip     *store.Clip
	started  time.Time
	progress float64
	moved    time.Time // last time progress changed
}

func (e *Engine) encodeStarted(c *store.Clip) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.live == nil {
		e.live = map[int64]*liveEncode{}
	}
	now := time.Now()
	e.live[c.ID] = &liveEncode{clip: c, started: now, moved: now}
	e.Hub.Publish("queue", "")
}

func (e *Engine) encodeProgress(c *store.Clip, f float64) {
	e.mu.Lock()
	if l := e.live[c.ID]; l != nil && f > l.progress {
		l.progress, l.moved = f, time.Now()
	}
	e.mu.Unlock()
	e.Hub.Throttled("queue", "", 2*time.Second)
}

func (e *Engine) encodeEnded(c *store.Clip) {
	e.mu.Lock()
	delete(e.live, c.ID)
	e.mu.Unlock()
	e.Hub.Publish("queue", "")
}

// encodeSpeed records seconds of video encoded per second of wall time.
func (e *Engine) encodeSpeed(video float64, wall time.Duration) {
	if video <= 0 || wall <= 0 {
		return
	}
	e.mu.Lock()
	e.speeds = append(e.speeds, video/wall.Seconds())
	if len(e.speeds) > 20 {
		e.speeds = e.speeds[len(e.speeds)-20:]
	}
	e.mu.Unlock()
}

// imageMoved records imaging progress for stall detection.
func (e *Engine) imageMoved(id string) {
	e.mu.Lock()
	if e.imaged == nil {
		e.imaged = map[string]time.Time{}
	}
	e.imaged[id] = time.Now()
	e.mu.Unlock()
}

// Overview summarizes everything in flight, for the dashboard.
type Overview struct {
	Reading     []Brief     `json:"reading"`    // a disc is being read
	Waiting     []Brief     `json:"waiting"`    // needs the user: answer, flip, paused
	Tagging     []Brief     `json:"tagging"`    // finished discs whose tags are being rewritten
	Encoding    []Encoding  `json:"encoding"`   // conversions running now
	Discs       []DiscQueue `json:"discs"`      // discs with clips still to convert
	ClipsLeft   int         `json:"clips_left"` // across all discs
	MinutesLeft float64     `json:"minutes_left"`
	Speed       float64     `json:"speed"` // video seconds per wall second, recent average
	Measured    bool        `json:"measured"`
	ETA         time.Time   `json:"eta,omitzero"`
	Stuck       []string    `json:"stuck,omitempty"`
	DoneToday   int         `json:"done_today"`
	Settings    string      `json:"settings"`
}

// Brief names a disc and what it's doing.
type Brief struct {
	ID    string  `json:"id"`
	Title string  `json:"title"`
	State string  `json:"state"`
	Note  string  `json:"note,omitempty"`
	Frac  float64 `json:"frac"`
}

// Encoding is one running conversion.
type Encoding struct {
	DiscID  string    `json:"disc_id"`
	Title   string    `json:"title"`
	Clip    string    `json:"clip"`
	Frac    float64   `json:"frac"`
	Started time.Time `json:"started"`
}

// DiscQueue is one disc's conversion progress.
type DiscQueue struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Done        int     `json:"done"`
	Total       int     `json:"total"`
	Failed      int     `json:"failed"`
	MinutesLeft float64 `json:"minutes_left"`
}

// defaultSpeed is assumed before any encode has finished (x264 medium, 50p,
// on a dual-core laptop CPU).
const defaultSpeed = 0.45

// Overview builds the dashboard summary.
func (e *Engine) Overview() Overview {
	var o Overview
	set := e.Settings()
	speed := set.Speed
	if speed != "best" && speed != "fast" {
		speed = "balanced"
	}
	o.Settings = set.Preset + " quality, " + speed + " speed"
	if set.HWAccel != "" {
		o.Settings += ", " + set.HWAccel
	}

	e.mu.Lock()
	o.Speed, o.Measured = defaultSpeed, false
	if n := len(e.speeds); n > 0 {
		var sum float64
		for _, s := range e.speeds {
			sum += s
		}
		o.Speed, o.Measured = sum/float64(n), true
	}
	now := time.Now()
	for _, l := range e.live {
		o.Encoding = append(o.Encoding, Encoding{DiscID: l.clip.DiscID, Clip: fmt.Sprintf("%s%02d", l.clip.Side, l.clip.Num), Frac: l.progress, Started: l.started})
		if now.Sub(l.moved) > StallAfter {
			o.Stuck = append(o.Stuck, fmt.Sprintf("Clip %s%02d of disc %s hasn't progressed for %d min.", l.clip.Side, l.clip.Num, l.clip.DiscID, int(now.Sub(l.moved).Minutes())))
		}
	}
	imaged := map[string]time.Time{}
	for k, v := range e.imaged {
		imaged[k] = v
	}
	e.mu.Unlock()

	discs, _ := e.St.Discs()
	titles := map[string]string{}
	today := time.Now().Truncate(24 * time.Hour)
	for _, d := range discs {
		title := d.Description
		if title == "" {
			title = d.Folder
		}
		if title == "" {
			title = "Disc " + d.ID[2:]
		}
		titles[d.ID] = title
		if d.State == store.Done && d.DoneAt.After(today) {
			o.DoneToday++
		}
		switch d.State {
		case store.Detected, store.Probing, store.ImagingA, store.ImagingB:
			b := Brief{ID: d.ID, Title: title, State: d.State.Label()}
			for _, s := range d.Sides {
				if !s.Imaged {
					b.Frac = s.ImageDone
				}
			}
			if t, ok := imaged[d.ID]; ok && now.Sub(t) > StallAfter && e.Running(d.ID) {
				o.Stuck = append(o.Stuck, fmt.Sprintf("Reading %s hasn't progressed for %d min (scratched disc? ddrescue retries can be slow).", title, int(now.Sub(t).Minutes())))
			}
			o.Reading = append(o.Reading, b)
		case store.AwaitingAnswer, store.AwaitingFlip, store.Duplicate, store.Paused:
			o.Waiting = append(o.Waiting, Brief{ID: d.ID, Title: title, State: d.State.Label(), Note: d.Message})
		}
		if d.State == store.Done && d.Retagging() {
			frac := 0.0
			if d.RetagTotal > 0 {
				frac = float64(d.RetagDone) / float64(d.RetagTotal)
			}
			o.Tagging = append(o.Tagging, Brief{ID: d.ID, Title: title, State: "Updating tags",
				Note: fmt.Sprintf("%d of %d files", d.RetagDone, d.RetagTotal), Frac: frac})
		}
		if d.State.Terminal() || d.State == store.Duplicate {
			continue
		}
		clips, _ := e.St.Clips(d.ID)
		q := DiscQueue{ID: d.ID, Title: title, Total: len(clips)}
		for _, c := range clips {
			switch {
			case c.Done():
				q.Done++
			case c.State == "failed":
				q.Failed++
			default:
				q.MinutesLeft += clipSeconds(c) * (1 - c.Progress) / 60
			}
		}
		if q.Total > 0 && q.Done+q.Failed < q.Total {
			o.Discs = append(o.Discs, q)
			o.ClipsLeft += q.Total - q.Done - q.Failed
			o.MinutesLeft += q.MinutesLeft
			// Work that should be running but isn't: a goroutine ended
			// without finishing (e.g. an error after a restart).
			if d.State == store.Processing && !e.Running(d.ID) {
				o.Stuck = append(o.Stuck, fmt.Sprintf("%s has %d clips left but nothing is working on it. Press Resume on its card.", title, q.Total-q.Done-q.Failed))
			}
		}
	}
	for i := range o.Encoding {
		o.Encoding[i].Title = titles[o.Encoding[i].DiscID]
	}
	sort.Slice(o.Encoding, func(i, j int) bool { return o.Encoding[i].Started.Before(o.Encoding[j].Started) })
	if o.MinutesLeft > 0 && o.Speed > 0 {
		o.ETA = now.Add(time.Duration(o.MinutesLeft * 60 / o.Speed * float64(time.Second)))
	}
	return o
}

// clipSeconds is a clip's length: probed, known from the disc, or estimated
// from the source size at a typical camcorder bitrate (~5 Mb/s).
func clipSeconds(c *store.Clip) float64 {
	if c.Info.Duration > 0 {
		return c.Info.Duration
	}
	if c.Source.Duration > 0 {
		return c.Source.Duration
	}
	var size int64
	for _, p := range append([]string{c.Source.Path}, c.Source.Files...) {
		if fi, err := os.Stat(p); err == nil && p != "" && !fi.IsDir() {
			size += fi.Size()
		}
	}
	return float64(size) / 625_000
}
