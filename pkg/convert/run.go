package convert

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mimic890/mekiki/internal/fsutil"
	"github.com/Mimic890/mekiki/pkg/probe"
	"github.com/Mimic890/mekiki/pkg/tools"
)

// Job is one input file converted to one output file.
type Job struct {
	ID          int    `json:"jobId"`
	Input       string `json:"input"`
	Output      string `json:"output"`
	Preset      Preset `json:"-"`
	Overwrite   bool   `json:"-"`
	OnlySmaller bool   `json:"-"` // keep the output only if it is smaller than the input
}

// ErrNotSmaller reports that an OnlySmaller job wrote nothing because the result was not smaller.
var ErrNotSmaller = errors.New("the result is not smaller, nothing written")

// Exif actions, run with exiftool on the finished output.
const (
	ExifCopy  = "copy"
	ExifStrip = "strip"
)

// ExifArgs returns the exiftool arguments of a Preset.Exif action, nil when
// there is none. Paths are made absolute so they can never be mistaken for options.
// The orientation survives a strip because -noautorotate leaves the pixels unrotated.
func ExifArgs(action, input, output string) ([]string, error) {
	if action == "" {
		return nil, nil
	}
	in, err := filepath.Abs(input)
	if err != nil {
		return nil, err
	}
	out, err := filepath.Abs(output)
	if err != nil {
		return nil, err
	}
	if action == ExifStrip {
		return []string{"-q", "-q", "-m", "-overwrite_original", "-all=", "--icc_profile:all", "-TagsFromFile", in, "-Orientation", out}, nil
	}
	return []string{"-q", "-q", "-m", "-overwrite_original", "-TagsFromFile", in, "-all:all", "-icc_profile", out}, nil
}

// Progress is a snapshot of a running ffmpeg job.
type Progress struct {
	OutTimeMs  int64   `json:"outTimeMs"`
	DurationMs int64   `json:"durationMs"` // 0 when unknown
	Percent    float64 `json:"percent"`    // 0 when the duration is unknown
	FPS        float64 `json:"fps"`
	Speed      float64 `json:"speed"`
	SizeBytes  int64   `json:"sizeBytes"`
}

// Run converts one file. ffmpeg writes to a hidden temp file next to the
// output, which is renamed into place only on success and removed otherwise.
// An existing output (including the input itself) is replaced only with job.Overwrite.
func Run(ctx context.Context, job Job, threads int, onProgress func(Progress)) (err error) {
	ffmpeg, err := tools.Path("ffmpeg")
	if err != nil {
		return err
	}
	if _, err := os.Stat(job.Output); err == nil && !job.Overwrite {
		return fmt.Errorf("%s %w", job.Output, fsutil.ErrExists)
	}

	var durationMs int64
	var fps float64
	var cover *probe.VideoStream
	job.Preset.attach, job.Preset.covers, job.Preset.videos = nil, nil, 0
	pctx, cancel := context.WithTimeout(ctx, probe.DefaultTimeout)
	if info, err := probe.ScanFileContext(pctx, job.Input); err == nil {
		durationMs = info.DurationMs
		for i, v := range info.Video {
			if !v.IsAttachedPic && fps == 0 {
				fps = v.FPS
			}
			if v.IsAttachedPic && cover == nil {
				cover = &info.Video[i]
			}
		}
		job.Preset.attach = attachments(info, job.Preset.Tracks, job.Output)
		if v := job.Preset.Video.Codec; job.Preset.attach == nil && job.Preset.VideoStream == "" && v != "" && v != "copy" {
			job.Preset.covers, job.Preset.videos = lastCovers(info, job.Preset.Tracks) // not Matroska: see BuildArgs
		}
	}
	cancel()
	if len(job.Preset.attach) > 0 {
		dir, err := os.MkdirTemp("", "mekiki-cover-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		for i := range job.Preset.attach {
			a := &job.Preset.attach[i]
			data, err := extractPicture(ctx, ffmpeg, job.Input, a.index)
			if err != nil {
				return err
			}
			a.path = filepath.Join(dir, a.name) // ffmpeg names the attachment after the file
			if err := os.WriteFile(a.path, data, 0o600); err != nil {
				return err
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(job.Output), 0o755); err != nil {
		return err
	}
	tmp, err := fsutil.TempPath(job.Output)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	args, err := BuildArgs(job.Preset, job.Input, tmp, threads)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if job.Preset.CoverTag {
		if cover == nil {
			return fmt.Errorf("%s: cover art not found", filepath.Base(job.Input))
		}
		tags, err := coverMetadata(ctx, ffmpeg, job.Input, *cover)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(tags)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	readProgress(stdout, durationMs, fps, onProgress)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("ffmpeg failed on %q: %s", filepath.Base(job.Input), ffmpegError(stderr.String(), err))
	}
	if err := runExif(ctx, job, tmp); err != nil {
		return err
	}
	if job.OnlySmaller {
		before, err := os.Stat(job.Input)
		if err != nil {
			return err
		}
		after, err := os.Stat(tmp)
		if err != nil {
			return err
		}
		if after.Size() >= before.Size() {
			os.Remove(tmp)
			return ErrNotSmaller
		}
	}
	return fsutil.Commit(tmp, job.Output, job.Overwrite)
}

// runExif applies the preset's exiftool step to the finished temp file.
func runExif(ctx context.Context, job Job, tmp string) error {
	args, err := ExifArgs(job.Preset.Exif, job.Input, tmp)
	if args == nil || err != nil {
		return err
	}
	exiftool, err := tools.Path("exiftool")
	if err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, exiftool, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("exiftool failed on %q: %s", filepath.Base(job.Input), strings.TrimSpace(string(out)))
	}
	return nil
}

// coverMetadata returns an ffmetadata document holding the file's cover art as a
// METADATA_BLOCK_PICTURE comment. Ogg muxers cannot store a picture stream, and
// a picture is too large for a -metadata argument, so ffmpeg reads this on stdin.
func coverMetadata(ctx context.Context, ffmpeg, input string, cover probe.VideoStream) ([]byte, error) {
	data, err := extractPicture(ctx, ffmpeg, input, cover.Index)
	if err != nil {
		return nil, err
	}
	// The FLAC picture block: type, MIME, description, width, height, depth, colors, data.
	mime := "image/" + strings.Replace(cover.Codec, "mjpeg", "jpeg", 1)
	block := binary.BigEndian.AppendUint32(nil, 3) // front cover
	block = binary.BigEndian.AppendUint32(block, uint32(len(mime)))
	block = append(block, mime...)
	block = binary.BigEndian.AppendUint32(block, 0)
	for _, n := range []int{cover.Width, cover.Height, 0, 0, len(data)} {
		block = binary.BigEndian.AppendUint32(block, uint32(n))
	}
	block = append(block, data...)
	encoded := strings.ReplaceAll(base64.StdEncoding.EncodeToString(block), "=", `\=`)
	return []byte(";FFMETADATA1\nMETADATA_BLOCK_PICTURE=" + encoded + "\n"), nil
}

// ffmpegError returns the last lines ffmpeg printed, which hold the actual reason.
func ffmpegError(stderr string, err error) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return err.Error()
	}
	return strings.Join(lines[max(len(lines)-3, 0):], "; ")
}

// readProgress parses ffmpeg -progress key=value blocks, calling fn once per block.
// ffmpeg reports out_time of the slowest stream, so a sparse subtitle track
// holds it back; with a known frame rate the video frame count is used too.
func readProgress(r io.Reader, durationMs int64, fps float64, fn func(Progress)) {
	p := Progress{DurationMs: durationMs}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, _ := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		switch key {
		case "out_time_us":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 {
				p.OutTimeMs = max(p.OutTimeMs, n/1000)
			}
		case "frame":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil && fps > 0 {
				p.OutTimeMs = max(p.OutTimeMs, int64(float64(n)*1000/fps))
			}
		case "fps":
			p.FPS, _ = strconv.ParseFloat(val, 64)
		case "speed":
			p.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(val), "x"), 64)
		case "total_size":
			p.SizeBytes, _ = strconv.ParseInt(val, 10, 64)
		case "progress":
			if durationMs > 0 {
				p.Percent = math.Min(100, math.Round(float64(p.OutTimeMs)*1000/float64(durationMs))/10)
			}
			if val == "end" {
				p.Percent = 100
			}
			if fn != nil {
				fn(p)
			}
		}
	}
}

// ErrNoEncoder reports that ffmpeg was built without an encoder a preset needs.
var ErrNoEncoder = errors.New("is not included in this ffmpeg build")

// Verify runs a short test encode with p to prove its encoders work on this
// machine; a listed encoder may still lack the hardware or driver behind it.
func Verify(ctx context.Context, p Preset) error {
	ffmpeg, err := tools.Path("ffmpeg")
	if err != nil {
		return err
	}
	available, err := listEncoders(ffmpeg)
	if err != nil {
		return err
	}
	for _, c := range []Codec{p.Video, p.Audio, p.Subtitle} {
		if !isCopy(c) && !available[c.Codec] {
			return fmt.Errorf("encoder %s %w", c.Codec, ErrNoEncoder)
		}
	}
	p.InputArgs = append(append([]string(nil), p.InputArgs...), "-f", "lavfi", "-t", "0.3")
	p.Args = append(append([]string(nil), p.Args...), "-f", "null")
	p.Tracks = nil
	// No ":" in the graph: BuildArgs would take it for a file name like "pipe:1".
	args, err := BuildArgs(p, "testsrc2=size=256x256", "-", 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("test encode timed out")
		}
		return errors.New(ffmpegError(stderr.String(), err))
	}
	return nil
}

// listEncoders returns the encoder names ffmpeg was built with, cached per path.
func listEncoders(ffmpeg string) (map[string]bool, error) {
	encodersMu.Lock()
	defer encodersMu.Unlock()
	if encoders[ffmpeg] != nil {
		return encoders[ffmpeg], nil
	}
	out, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders: %w", err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like " V....D libx264   libx264 H.264 ..."; the legend lines have "=" in field 2.
		if f := strings.Fields(line); len(f) >= 2 && len(f[0]) == 6 && f[1] != "=" {
			names[f[1]] = true
		}
	}
	encoders[ffmpeg] = names
	return names, nil
}

var (
	encodersMu sync.Mutex
	encoders   = map[string]map[string]bool{}
)

// VerifyGPU test-encodes every GPU-class preset concurrently.
// The result maps each GPU preset name to nil (works) or the failure.
func VerifyGPU(ctx context.Context, presets []Preset) map[string]error {
	results := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range presets {
		if p.Load != LoadGPU {
			continue
		}
		wg.Add(1)
		go func(p Preset) {
			defer wg.Done()
			err := Verify(ctx, p)
			mu.Lock()
			results[p.Name] = err
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return results
}

// Available resolves every preset in the library and test-encodes the GPU
// ones. It returns the presets usable here and each GPU preset's result.
func (l *Library) Available(ctx context.Context) ([]Preset, map[string]error, error) {
	var all []Preset
	for _, name := range l.Names() {
		p, err := l.Get(name)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, p)
	}
	gpu := VerifyGPU(ctx, all)
	var ok []Preset
	for _, p := range all {
		if gpu[p.Name] == nil {
			ok = append(ok, p)
		}
	}
	return ok, gpu, ctx.Err()
}
