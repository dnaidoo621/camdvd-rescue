package store

import (
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/pipeline"
)

// State is a disc job's place in the state machine. The drive is held from
// Detected through ImagingB; everything after runs from the images.
type State string

const (
	Detected       State = "detected"
	Probing        State = "probing"
	ImagingA       State = "imaging-a"
	AwaitingAnswer State = "awaiting-answer" // side A imaged, sides unknown: tray stays closed
	AwaitingFlip   State = "awaiting-flip"
	ImagingB       State = "imaging-b"
	Processing     State = "processing" // classify, extract or carve, convert (per side)
	Finalizing     State = "finalizing"
	Done           State = "done"
	Failed         State = "failed"
	Cancelled      State = "cancelled"
	Paused         State = "paused"
	Duplicate      State = "duplicate" // already imported: waiting for "import again?"
)

// Terminal reports whether no work is pending for the state.
func (s State) Terminal() bool { return s == Done || s == Failed || s == Cancelled }

// HoldsDrive reports whether a job in this state owns its drive.
func (s State) HoldsDrive() bool {
	switch s {
	case Detected, Probing, ImagingA, AwaitingAnswer, AwaitingFlip, ImagingB, Duplicate:
		return true
	}
	return false
}

// Label is the plain-words state for the UI.
func (s State) Label() string {
	switch s {
	case ImagingA:
		return "Imaging side A"
	case ImagingB:
		return "Imaging side B"
	case AwaitingAnswer:
		return "Waiting for your answer"
	case AwaitingFlip:
		return "Flip the disc and insert side B"
	case Duplicate:
		return "Already imported"
	}
	b := []byte(s)
	if len(b) > 0 && b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

// Stage records one step of a side's processing.
type Stage struct {
	Name     string    `json:"name"`
	Status   string    `json:"status"` // running, done, failed, skipped
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Progress float64   `json:"progress"`
	Cmd      []string  `json:"cmd,omitempty"`
	Stderr   string    `json:"stderr,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// Side is one side of a disc.
type Side struct {
	Letter      string                 `json:"letter"`
	Fingerprint string                 `json:"fingerprint"`
	Probe       discinfo.Probe         `json:"probe"`
	Evidence    string                 `json:"evidence"`
	Method      discinfo.ImagingMethod `json:"method"`
	Sectors     int64                  `json:"sectors"`
	Bad         drive.Ranges           `json:"bad,omitempty"`
	ImageDone   float64                `json:"image_done"` // 0..1
	Imaged      bool                   `json:"imaged"`
	StoppedAt   int64                  `json:"stopped_at,omitempty"`
	Decision    discinfo.Decision      `json:"decision"`
	Step        string                 `json:"step"` // classifying, extracting, carving, verifying, converting, done
	Extracted   bool                   `json:"extracted"`
	Processed   bool                   `json:"processed"`
	Error       string                 `json:"error,omitempty"`
	Stages      []Stage                `json:"stages"`
}

// Readable is the fraction of the imaged area that read cleanly.
func (s Side) Readable() float64 {
	if s.Sectors == 0 {
		return 0
	}
	return 1 - float64(s.Bad.Total())/float64(s.Sectors)
}

// Disc is one disc job: both sides, the answers, and where it ended up.
type Disc struct {
	ID          string    `json:"id"`
	DriveID     string    `json:"drive_id"`
	State       State     `json:"state"`
	Message     string    `json:"message,omitempty"` // why paused/failed, or what to do
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	DoneAt      time.Time `json:"done_at,omitzero"`
	Answered    bool      `json:"answered"`
	SidesWanted int       `json:"sides_wanted"` // 1 or 2 once answered
	Description string    `json:"description"`
	Sides       []*Side   `json:"sides"`
	Folder      string    `json:"folder,omitempty"` // relative to the library
	Warnings    []string  `json:"warnings,omitempty"`
	ForceRaw    bool      `json:"force_raw,omitempty"`
	DuplicateOf string    `json:"duplicate_of,omitempty"`

	// Per-disc metadata, editable in the library.
	Make     string `json:"make,omitempty"`
	Model    string `json:"model,omitempty"`
	TZ       string `json:"tz,omitempty"`
	DiscDate string `json:"disc_date,omitempty"` // yyyy-mm-dd or yyyy-mm-ddThh:mm
	Location string `json:"location,omitempty"`  // ISO 6709

	Imported bool   `json:"imported,omitempty"` // ticked off as imported into Photos
	Cleaned  bool   `json:"cleaned,omitempty"`  // image and sources removed
	Combined string `json:"combined,omitempty"`
}

// Side returns the side with the letter, or nil.
func (d *Disc) Side(letter string) *Side {
	for _, s := range d.Sides {
		if s.Letter == letter {
			return s
		}
	}
	return nil
}

// Clip is one recording and its output.
type Clip struct {
	ID       int64             `json:"id"`
	DiscID   string            `json:"disc_id"`
	Side     string            `json:"side"`
	Num      int               `json:"num"` // 1-based within the side
	Source   pipeline.Source   `json:"source"`
	Info     pipeline.Info     `json:"info"`
	State    string            `json:"state"` // pending, converting, converted, placed, failed
	Progress float64           `json:"progress"`
	Work     string            `json:"work,omitempty"`   // converted file before placing, absolute
	Output   string            `json:"output,omitempty"` // relative to the library once placed
	Thumb    string            `json:"thumb,omitempty"`  // absolute
	Warnings []string          `json:"warnings,omitempty"`
	Error    string            `json:"error,omitempty"`
	SHA256   string            `json:"sha256,omitempty"`
	Size     int64             `json:"size,omitempty"`
	Date     pipeline.ClipDate `json:"date"`
}

// Done reports whether conversion finished.
func (c Clip) Done() bool { return c.State == "converted" || c.State == "placed" }

// RenameBatch is an applied bulk rename, kept for undo.
type RenameBatch struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Summary   string    `json:"summary"`
	Moves     []Move    `json:"moves"`
	Undone    bool      `json:"undone"`
}

// Move mirrors library.Move without importing it here.
type Move struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}
