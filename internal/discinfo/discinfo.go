// Package discinfo parses what the drive reports about a disc and decides
// how it must be read. It has no I/O, so the decision table is unit-tested.
package discinfo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Media is the parsed output of dvd+rw-mediainfo.
type Media struct {
	Type         string `json:"type"`          // e.g. "DVD-R Sequential", "DVD-RAM"
	DiscStatus   string `json:"disc_status"`   // blank, appendable, complete, other
	SessionState string `json:"session_state"` // empty, incomplete, complete
	// TrackStart and NextWritable bound the written area of an open session,
	// in 2048-byte sectors. NextWritable is 0 when the drive doesn't report it.
	TrackStart   int64 `json:"track_start"`
	NextWritable int64 `json:"next_writable"`
	// Capacity is READ CAPACITY in sectors; for an open session the kernel
	// value may be wrong, which is why raw reads use NextWritable instead.
	Capacity int64 `json:"capacity"`
	NoMedia  bool  `json:"no_media"`
	// Tracks are the drive's track table. A camcorder's open DVD-R has
	// several, with unwritten gaps between them.
	Tracks []Track `json:"tracks,omitempty"`
}

// Track is one entry of READ TRACK INFORMATION, in sectors.
type Track struct {
	State        string `json:"state"`
	Start        int64  `json:"start"`
	Size         int64  `json:"size"`
	NextWritable int64  `json:"next_writable,omitempty"`
	LastRecorded int64  `json:"last_recorded,omitempty"`
}

// Extent is a written sector range [Start, End).
type Extent struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// written returns the track's written range, or false if nothing is written.
func (t Track) written() (Extent, bool) {
	switch {
	case t.LastRecorded >= t.Start && t.LastRecorded > 0:
		return Extent{t.Start, t.LastRecorded + 1}, true
	case t.NextWritable > t.Start:
		return Extent{t.Start, t.NextWritable}, true
	case strings.HasPrefix(t.State, "complete") && t.Size > 0:
		return Extent{t.Start, t.Start + t.Size}, true
	}
	return Extent{}, false
}

// Extents lists the written ranges to read from an open disc, in order.
// Without a track table it falls back to [0, UsedSectors).
func (m Media) Extents() []Extent {
	var out []Extent
	for _, t := range m.Tracks {
		if e, ok := t.written(); ok {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		if used := m.UsedSectors(); used > 0 {
			out = []Extent{{0, used}}
		}
	}
	return out
}

var (
	reMounted  = regexp.MustCompile(`(?m)^\s*Mounted Media:\s*[0-9A-Fa-f]+h,\s*(.+?)\s*$`)
	reStatus   = regexp.MustCompile(`(?m)^\s*Disc status:\s*(\S+)`)
	reSession  = regexp.MustCompile(`(?m)^\s*State of Last Session:\s*(\S+)`)
	reStart    = regexp.MustCompile(`(?m)^\s*Track Start Address:\s*(\d+)\*2KB`)
	reNext     = regexp.MustCompile(`(?m)^\s*Next Writable Address:\s*(\d+)\*2KB`)
	reCapacity = regexp.MustCompile(`(?m)^\s*READ CAPACITY:\s*(\d+)\*2048`)
	reTrackHdr = regexp.MustCompile(`(?m)^READ TRACK INFORMATION\[#\d+\]:`)
	reTState   = regexp.MustCompile(`(?m)^\s*Track State:\s*(.+?)\s*$`)
	reTSize    = regexp.MustCompile(`(?m)^\s*Track Size:\s*(\d+)\*2KB`)
	reLastRec  = regexp.MustCompile(`(?m)^\s*Last Recorded Address:\s*(\d+)\*2KB`)
)

func parseTracks(out string) []Track {
	idx := reTrackHdr.FindAllStringIndex(out, -1)
	var ts []Track
	for i, loc := range idx {
		end := len(out)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		blk := out[loc[1]:end]
		// The block ends where the next section header starts.
		if j := regexp.MustCompile(`(?m)^[A-Z][A-Z ]+[\[:]`).FindStringIndex(blk); j != nil {
			blk = blk[:j[0]]
		}
		var t Track
		num := func(re *regexp.Regexp) int64 {
			if s := re.FindStringSubmatch(blk); s != nil {
				n, _ := strconv.ParseInt(s[1], 10, 64)
				return n
			}
			return 0
		}
		if s := reTState.FindStringSubmatch(blk); s != nil {
			t.State = strings.ToLower(s[1])
		}
		t.Start, t.Size, t.NextWritable, t.LastRecorded = num(reStart), num(reTSize), num(reNext), num(reLastRec)
		ts = append(ts, t)
	}
	return ts
}

// ParseMediaInfo reads dvd+rw-mediainfo output. A disc with several tracks
// lists each; the last track's addresses are kept, because that's the open
// one on an unfinalized disc.
func ParseMediaInfo(out string) Media {
	var m Media
	if strings.Contains(out, "no media mounted") {
		m.NoMedia = true
		return m
	}
	if s := reMounted.FindStringSubmatch(out); s != nil {
		m.Type = s[1]
	}
	if s := reStatus.FindStringSubmatch(out); s != nil {
		m.DiscStatus = strings.ToLower(s[1])
	}
	if s := reSession.FindStringSubmatch(out); s != nil {
		m.SessionState = strings.ToLower(s[1])
	}
	if all := reStart.FindAllStringSubmatch(out, -1); all != nil {
		m.TrackStart, _ = strconv.ParseInt(all[len(all)-1][1], 10, 64)
	}
	if all := reNext.FindAllStringSubmatch(out, -1); all != nil {
		m.NextWritable, _ = strconv.ParseInt(all[len(all)-1][1], 10, 64)
	}
	if s := reCapacity.FindStringSubmatch(out); s != nil {
		m.Capacity, _ = strconv.ParseInt(s[1], 10, 64)
	}
	m.Tracks = parseTracks(out)
	return m
}

// IsDVD reports whether the media is any DVD format.
func (m Media) IsDVD() bool { return strings.HasPrefix(strings.ToUpper(m.Type), "DVD") }

// IsRAM reports DVD-RAM, which has no sessions and never needs finalizing.
func (m Media) IsRAM() bool { return strings.Contains(strings.ToUpper(m.Type), "DVD-RAM") }

// Open reports an unfinalized disc: appendable, last session not closed.
func (m Media) Open() bool {
	return !m.IsRAM() && m.DiscStatus == "appendable" && m.SessionState != "complete"
}

// UsedSectors is the size of the written area to read.
func (m Media) UsedSectors() int64 {
	if m.Open() {
		var end int64
		for _, t := range m.Tracks {
			if e, ok := t.written(); ok {
				end = max(end, e.End)
			}
		}
		if end > 0 {
			return max(end, m.NextWritable)
		}
	}
	if m.Open() && m.NextWritable > 0 {
		return m.NextWritable
	}
	if m.Capacity > 0 {
		return m.Capacity
	}
	return m.NextWritable
}

// Class is the outcome of classification.
type Class string

const (
	Blank       Class = "blank"
	Finalized   Class = "finalized"    // DVD-Video (VIDEO_TS)
	FinalizedVR Class = "finalized-vr" // DVD-VR (DVD_RTAV) on DVD-RW
	RAM         Class = "dvd-ram"      // DVD-VR on DVD-RAM
	Unfinalized Class = "unfinalized"  // open session, raw recovery
	Damaged     Class = "damaged"      // file system present but unusable
	DataDisc    Class = "data"         // file system without video
	Unsupported Class = "unsupported"  // not a DVD
)

// Path is the extraction pipeline a class takes.
type Path string

const (
	PathNone   Path = "none"
	PathFiles  Path = "files"
	PathRaw    Path = "raw"
	PathCopy   Path = "copy-only"
	PathReject Path = "reject"
)

// Path returns the pipeline for c.
func (c Class) Path() Path {
	switch c {
	case Finalized, FinalizedVR, RAM:
		return PathFiles
	case Unfinalized, Damaged:
		return PathRaw
	case DataDisc:
		return PathCopy
	case Unsupported:
		return PathReject
	}
	return PathNone
}

// Probe is the evidence classification is based on. FSType comes from
// blkid; HasVideoTS and HasRTAV from listing the image after imaging.
type Probe struct {
	Media      Media  `json:"media"`
	FSType     string `json:"fs_type"`
	HasVideoTS bool   `json:"has_video_ts"`
	HasRTAV    bool   `json:"has_rtav"`
	// Listed is true once the image's file system has been listed, so the
	// VIDEO_TS / DVD_RTAV flags mean something.
	Listed bool `json:"listed"`
	// ListFailed means a file system was found but couldn't be read.
	ListFailed bool `json:"list_failed"`
}

// Decision is a classification with a plain-language reason.
type Decision struct {
	Class  Class  `json:"class"`
	Reason string `json:"reason"`
}

// ImagingMethod says how to read a side before it's classified in full.
type ImagingMethod string

const (
	ImageBlock ImagingMethod = "ddrescue" // whole disc through /dev/srN
	ImageRaw   ImagingMethod = "sg_dd"    // LBA 0 to next-writable through /dev/sgN
)

// PreClassify runs on the probe before any read: it rejects blank and
// non-DVD media and picks the imaging method.
func PreClassify(p Probe) (Decision, ImagingMethod, bool) {
	m := p.Media
	switch {
	case m.NoMedia:
		return Decision{Blank, "No disc in the drive"}, "", false
	case m.Type != "" && !m.IsDVD():
		return Decision{Unsupported, fmt.Sprintf("%s is not a DVD; only camcorder DVDs are supported", m.Type)}, "", false
	case m.DiscStatus == "blank" || (!m.IsRAM() && m.UsedSectors() == 0 && p.FSType == ""):
		return Decision{Blank, "Disc is blank"}, "", false
	case m.Open() && p.FSType == "":
		return Decision{}, ImageRaw, true
	}
	return Decision{}, ImageBlock, true
}

// Classify applies the spec's decision table once the image is listed.
func Classify(p Probe) Decision {
	m := p.Media
	if d, _, ok := PreClassify(p); !ok {
		return d
	}
	size := sizeText(m.UsedSectors())
	switch {
	case m.Open() && p.FSType == "":
		return Decision{Unfinalized, fmt.Sprintf("Unfinalized %s: no file system, %s of data", short(m), size)}
	case p.FSType != "" && p.ListFailed:
		return Decision{Damaged, fmt.Sprintf("%s has a %s file system that couldn't be read; recovering raw", short(m), strings.ToUpper(p.FSType))}
	case m.IsRAM() && p.HasRTAV:
		return Decision{RAM, fmt.Sprintf("DVD-RAM with DVD-VR recordings, %s", size)}
	case p.HasRTAV:
		return Decision{FinalizedVR, fmt.Sprintf("Finalized %s in DVD-VR format, %s", short(m), size)}
	case p.HasVideoTS:
		return Decision{Finalized, fmt.Sprintf("Finalized %s with DVD-Video, %s", short(m), size)}
	case p.FSType != "" && p.Listed:
		return Decision{DataDisc, fmt.Sprintf("%s with a %s file system but no video folders: a data disc", short(m), strings.ToUpper(p.FSType))}
	case p.FSType == "":
		return Decision{Damaged, fmt.Sprintf("%s reports %s but has no file system; recovering raw", short(m), m.DiscStatus)}
	}
	return Decision{Damaged, "Couldn't identify the disc layout; recovering raw"}
}

func short(m Media) string {
	t := m.Type
	if i := strings.IndexByte(t, ' '); i > 0 {
		t = t[:i]
	}
	if t == "" {
		return "disc"
	}
	return t
}

// sizeText gives a rough recording length, assuming the ~70 MB/min that
// camcorders average in their default quality mode.
func sizeText(sectors int64) string {
	mb := float64(sectors) * 2048 / 1e6
	min := mb / 70
	switch {
	case min < 1:
		return fmt.Sprintf("%.0f MB", mb)
	default:
		return fmt.Sprintf("about %.0f min", min+0.5)
	}
}
