package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// Info is what ffprobe says about a source.
type Info struct {
	Duration   float64 `json:"duration"`
	HasVideo   bool    `json:"has_video"`
	HasAudio   bool    `json:"has_audio"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	FieldOrder string  `json:"field_order"`
	DAR        string  `json:"dar"`
	FrameRate  string  `json:"frame_rate"`
	VideoCodec string  `json:"video_codec"`
	AudioCodec string  `json:"audio_codec"`
}

// Interlaced reports whether the source needs deinterlacing.
func (i Info) Interlaced() bool {
	switch i.FieldOrder {
	case "tt", "bb", "tb", "bt":
		return true
	}
	return false
}

// Probe runs ffprobe on a source.
func Probe(ctx context.Context, ts tools.Set, s Source) (Info, error) {
	args := append([]string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams"}, s.InputArgs()...)
	r, err := ts.Run(ctx, tools.Cmd{Name: "ffprobe", Args: args})
	if err != nil {
		return Info{}, err
	}
	var p struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			FieldOrder string `json:"field_order"`
			DAR        string `json:"display_aspect_ratio"`
			RFrameRate string `json:"r_frame_rate"`
			Duration   string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &p); err != nil {
		return Info{}, fmt.Errorf("ffprobe output: %w", err)
	}
	var in Info
	in.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	for _, st := range p.Streams {
		switch st.CodecType {
		case "video":
			if in.HasVideo {
				continue
			}
			in.HasVideo = true
			in.Width, in.Height, in.FieldOrder, in.DAR, in.FrameRate, in.VideoCodec = st.Width, st.Height, st.FieldOrder, st.DAR, st.RFrameRate, st.CodecName
			if d, _ := strconv.ParseFloat(st.Duration, 64); in.Duration == 0 {
				in.Duration = d
			}
		case "audio":
			if !in.HasAudio {
				in.HasAudio, in.AudioCodec = true, st.CodecName
			}
		}
	}
	return in, nil
}

// Verify checks a source the way the spec's stage 3 does: a video stream, a
// positive duration, and decodable first and last seconds. It returns a fatal
// problem, or warnings for a clip that's usable but imperfect.
func Verify(ctx context.Context, ts tools.Set, s Source, in Info) (problem string, warnings []string) {
	if !in.HasVideo {
		return "no video stream", nil
	}
	if in.Duration <= 0 {
		return "zero duration", nil
	}
	decode := func(seek float64) (bool, string) {
		args := []string{"-hide_banner", "-nostdin", "-v", "error"}
		if seek > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.2f", seek))
		}
		args = append(args, s.InputArgs()...)
		args = append(args, "-map", "0:v:0", "-t", "2", "-f", "null", "-")
		r, err := ts.Run(ctx, tools.Cmd{Name: "ffmpeg", Args: args})
		return err == nil, decodeErrors(r.Stderr)
	}
	ok, errs := decode(0)
	if !ok {
		return "first seconds don't decode: " + firstLine(errs), nil
	}
	if errs != "" {
		warnings = append(warnings, "decode errors near the start: "+firstLine(errs))
	}
	if in.Duration > 4 {
		ok, errs = decode(in.Duration - 3)
		if !ok {
			warnings = append(warnings, "last seconds don't decode: "+firstLine(errs))
		} else if errs != "" {
			warnings = append(warnings, "decode errors near the end: "+firstLine(errs))
		}
	}
	if s.BadSectors > 0 {
		warnings = append(warnings, fmt.Sprintf("%d unreadable sectors were zero-filled inside this clip", s.BadSectors))
	}
	return "", warnings
}

// decodeErrors drops libdvdread/libdvdnav chatter, which they log at error
// level when reading a VIDEO_TS folder rather than a device ("Couldn't find
// device name"), and keeps real decode errors.
func decodeErrors(stderr string) string {
	var keep []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "libdvdread:") || strings.Contains(line, "libdvdnav:") || strings.HasPrefix(line, "Last message repeated") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Preset names.
const (
	PresetArchive  = "archive"
	PresetStandard = "standard"
	PresetSmall    = "small"
	PresetCopy     = "copy"
)

// CRF for each preset.
var PresetCRF = map[string]int{PresetArchive: 16, PresetStandard: 18, PresetSmall: 22}

// Ext is the output extension for a preset.
func Ext(preset string) string {
	if preset == PresetCopy {
		return ".mkv"
	}
	return ".mp4"
}

// EncodeOptions configures one conversion.
type EncodeOptions struct {
	Preset  string
	HWAccel string // "", vaapi, nvenc
	Device  string // VAAPI render node
	Created time.Time
}

// ConvertArgs builds the FFmpeg command line for a conversion.
func ConvertArgs(s Source, in Info, o EncodeOptions, out string) []string {
	a := []string{"-hide_banner", "-nostdin", "-y", "-nostats", "-progress", "pipe:1", "-fflags", "+discardcorrupt+genpts"}
	if o.HWAccel == "vaapi" && o.Preset != PresetCopy {
		dev := o.Device
		if dev == "" {
			dev = "/dev/dri/renderD128"
		}
		a = append(a, "-vaapi_device", dev)
	}
	a = append(a, s.InputArgs()...)
	a = append(a, "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn")
	if !o.Created.IsZero() {
		a = append(a, "-metadata", "creation_time="+o.Created.UTC().Format("2006-01-02T15:04:05.000000Z"))
	}
	if o.Preset == PresetCopy {
		return append(a, "-c", "copy", "-f", "matroska", out)
	}
	var vf []string
	if in.Interlaced() {
		// One output frame per field: 50p for PAL, 59.94p for NTSC.
		vf = append(vf, "bwdif=mode=send_field:parity=auto:deint=all")
	}
	crf := PresetCRF[o.Preset]
	if crf == 0 {
		crf = PresetCRF[PresetStandard]
	}
	switch o.HWAccel {
	case "vaapi":
		vf = append(vf, "format=nv12", "hwupload")
		a = append(a, "-vf", strings.Join(vf, ","), "-c:v", "h264_vaapi", "-profile:v", "high", "-level", "41", "-rc_mode", "CQP", "-qp", strconv.Itoa(crf+2))
	case "nvenc":
		if len(vf) > 0 {
			a = append(a, "-vf", strings.Join(vf, ","))
		}
		a = append(a, "-c:v", "h264_nvenc", "-preset", "p6", "-profile:v", "high", "-level", "4.1", "-rc", "vbr", "-cq", strconv.Itoa(crf+1), "-pix_fmt", "yuv420p")
	default:
		if len(vf) > 0 {
			a = append(a, "-vf", strings.Join(vf, ","))
		}
		a = append(a, "-c:v", "libx264", "-preset", "slow", "-crf", strconv.Itoa(crf), "-profile:v", "high", "-level:v", "4.1", "-pix_fmt", "yuv420p")
	}
	a = append(a, "-c:a", "aac", "-b:a", "192k", "-ac", "2", "-movflags", "+faststart", "-f", "mp4", out)
	return a
}

// Convert encodes a source, reporting progress as a fraction 0..1. It
// writes to out+".part" and renames on success, so a half-written file is
// never mistaken for a finished one.
func Convert(ctx context.Context, ts tools.Set, s Source, in Info, o EncodeOptions, out string, progress func(float64)) (tools.Result, error) {
	part := out + ".part"
	args := ConvertArgs(s, in, o, part)
	r, err := ts.Run(ctx, tools.Cmd{Name: "ffmpeg", Args: args, OnLine: func(line string) {
		if v, ok := strings.CutPrefix(line, "out_time_us="); ok && in.Duration > 0 && progress != nil {
			if us, err := strconv.ParseFloat(v, 64); err == nil {
				progress(min(1, us/1e6/in.Duration))
			}
		}
	}})
	if err != nil {
		os.Remove(part)
		return r, err
	}
	return r, os.Rename(part, out)
}

// Thumbnail grabs a frame a fifth of the way in.
func Thumbnail(ctx context.Context, ts tools.Set, video string, duration float64, jpg string) error {
	if err := os.MkdirAll(filepath.Dir(jpg), 0o755); err != nil {
		return err
	}
	_, err := ts.Run(ctx, tools.Cmd{Name: "ffmpeg", Args: []string{"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-ss", fmt.Sprintf("%.2f", duration/5), "-i", video, "-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "4", jpg}})
	return err
}

// Tags are the Apple Photos fields written into each MP4.
type Tags struct {
	When        time.Time // recording moment, in its own zone; zero = unknown
	Make, Model string
	Title       string
	Description string
	Keywords    []string
	Location    string // ISO 6709, e.g. +29.8587+031.0218/
	Comment     string // disc id, side and clip, for re-linking
}

// TagArgs builds the ExifTool arguments for t.
func TagArgs(t Tags, file string) []string {
	a := []string{"-overwrite_original", "-m"}
	set := func(tag, v string) {
		if v != "" {
			a = append(a, "-"+tag+"="+v)
		}
	}
	if !t.When.IsZero() {
		local := t.When.Format("2006:01:02 15:04:05-07:00")
		utc := t.When.UTC().Format("2006:01:02 15:04:05")
		set("Keys:CreationDate", local)
		for _, tag := range []string{"QuickTime:CreateDate", "QuickTime:ModifyDate", "QuickTime:TrackCreateDate", "QuickTime:TrackModifyDate", "QuickTime:MediaCreateDate", "QuickTime:MediaModifyDate"} {
			set(tag, utc)
		}
		set("FileModifyDate", local)
	}
	set("Keys:Make", t.Make)
	set("Keys:Model", t.Model)
	set("Keys:Title", t.Title)
	set("Keys:Description", t.Description)
	if len(t.Keywords) > 0 {
		set("Keys:Keywords", strings.Join(t.Keywords, ", "))
	}
	set("Keys:LocationISO6709", t.Location)
	set("Keys:Comment", t.Comment)
	return append(a, file)
}

// Tag writes t into an MP4 in place. Re-running it after a date fix rewrites
// the tags without re-encoding.
func Tag(ctx context.Context, ts tools.Set, t Tags, file string) (tools.Result, error) {
	return ts.Run(ctx, tools.Cmd{Name: "exiftool", Args: TagArgs(t, file)})
}

// ReadComment returns an MP4's Keys:Comment, used to re-link renamed files.
func ReadComment(ctx context.Context, ts tools.Set, file string) string {
	out, _ := ts.Output(ctx, "exiftool", "-s3", "-Keys:Comment", file)
	return strings.TrimSpace(out)
}
