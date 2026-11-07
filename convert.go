package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"

	"github.com/Mimic890/mekiki/internal/fsutil"
	"github.com/Mimic890/mekiki/pkg/convert"
	"github.com/Mimic890/mekiki/pkg/tools"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// presetFlags are the codec flags shared by convert and preset save.
type presetFlags struct {
	fs                                     *flag.FlagSet
	vcodec, acodec, scodec, vbitrate       string
	abitrate, encPreset, pixFmt, container string
	load                                   string
	crf                                    int
	tracks, rawArgs                        stringList
}

func addPresetFlags(fs *flag.FlagSet) *presetFlags {
	f := &presetFlags{fs: fs}
	fs.StringVar(&f.vcodec, "vcodec", "", "video encoder, e.g. libx265, hevc_nvenc or copy")
	fs.StringVar(&f.acodec, "acodec", "", "audio encoder, e.g. libopus or copy")
	fs.StringVar(&f.scodec, "scodec", "", "subtitle encoder, e.g. copy or mov_text")
	fs.IntVar(&f.crf, "crf", 0, "constant quality for the main stream (mapped per encoder)")
	fs.StringVar(&f.vbitrate, "vbitrate", "", "video bitrate, e.g. 6M")
	fs.StringVar(&f.abitrate, "abitrate", "", "audio bitrate, e.g. 192k")
	fs.StringVar(&f.encPreset, "encoder-preset", "", "video encoder speed preset, e.g. slow or p5")
	fs.StringVar(&f.pixFmt, "pix-fmt", "", "video pixel format, e.g. yuv420p10le")
	fs.StringVar(&f.container, "container", "", "output extension when writing to a directory, e.g. mkv")
	fs.StringVar(&f.load, "load", "", "load class: cpu-heavy, light, gpu or io")
	fs.Var(&f.tracks, "track", "track to keep: all, N, v, a, s, t, TYPE:N or TYPE:all (repeatable; default all)")
	fs.Var(&f.rawArgs, "ffmpeg-arg", "extra raw ffmpeg output argument (repeatable)")
	return f
}

// apply copies the flags the user set onto p. crf targets audio for audio presets.
func (f *presetFlags) apply(p *convert.Preset, media string) {
	f.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "vcodec":
			p.Video.Codec = f.vcodec
		case "acodec":
			p.Audio.Codec = f.acodec
		case "scodec":
			p.Subtitle.Codec = f.scodec
		case "crf":
			crf := f.crf
			if media == "audio" {
				p.Audio.CRF = &crf
			} else {
				p.Video.CRF = &crf
			}
		case "vbitrate":
			p.Video.Bitrate = f.vbitrate
		case "abitrate":
			p.Audio.Bitrate = f.abitrate
		case "encoder-preset":
			p.Video.Preset = f.encPreset
		case "pix-fmt":
			p.Video.PixFmt = f.pixFmt
		case "container":
			p.Container = strings.TrimPrefix(f.container, ".")
		case "load":
			p.Load = f.load
		case "track":
			p.Tracks = f.tracks
		case "ffmpeg-arg":
			p.Args = append(append([]string(nil), p.Args...), f.rawArgs...)
		}
	})
}

func configPath(name string) (string, error) {
	dir, err := convert.ConfigDir()
	return filepath.Join(dir, name), err
}

func loadLibrary() (*convert.Library, error) {
	path, err := configPath("presets.yml")
	if err != nil {
		return nil, err
	}
	return convert.LoadLibrary(path)
}

// ── convert ───────────────────────────────────────────────────────────────────

type dryRunJob struct {
	convert.Job
	Preset  string   `json:"preset"`
	Load    string   `json:"load"`
	Threads int      `json:"threads"`
	Command []string `json:"command"`
	Post    []string `json:"post,omitempty"` // exiftool step after ffmpeg

	Plan *convert.FilePlan `json:"plan,omitempty"`
}

func (a *app) convert(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	out := fs.String("o", "", "output file, or directory for several inputs (required)")
	presetName := fs.String("preset", "", "preset name (see preset list); flags override its fields")
	jobsFlag := "auto"
	fs.StringVar(&jobsFlag, "jobs", "auto", "parallel jobs: a number or auto (per load class, see config.yml)")
	fs.StringVar(&jobsFlag, "j", "auto", "shorthand for --jobs")
	failFast := fs.Bool("fail-fast", false, "stop all jobs after the first failure")
	overwrite := fs.Bool("overwrite", false, "replace existing output files, including the input itself")
	dryRun := fs.Bool("dry-run", false, "print the ffmpeg commands without running them")
	recursive := fs.Bool("r", false, "take the subfolders of directory inputs too; -o keeps their tree")
	pf := addPresetFlags(fs)
	paths, err := a.parseFlags(fs, args, "convert [flags] <input...> -o <output>")
	if err != nil {
		return err
	}
	if len(paths) == 0 || *out == "" {
		return usageErr("convert: need at least one input and -o <output>")
	}
	jobs, err := parseJobs("convert", jobsFlag)
	if err != nil {
		return err
	}

	lib, err := loadLibrary()
	if err != nil {
		return err
	}
	p := convert.Preset{Name: "custom"}
	if *presetName != "" {
		if p, err = lib.Get(*presetName); errors.Is(err, convert.ErrUnknownPreset) {
			return usageErr("convert: %v", err)
		} else if err != nil {
			return err
		}
		if len(p.Tokens) > 0 {
			return usageErr("convert: preset %s holds tokens; use it with mekiki %s", p.Name, map[string]string{convert.MediaVideo: "video", convert.MediaPhoto: "photo", convert.MediaMusic: "music"}[p.Media])
		}
	}
	pf.apply(&p, p.Media)
	if err := p.Normalize(); err != nil {
		return usageErr("convert: %v", err)
	}

	inputs, subdirs, err := expandInputs(paths, out, *recursive)
	if err != nil {
		return err
	}
	planOut := *out
	if subdirs != nil {
		planOut = "" // then mirror
	}
	planned, err := convert.Plan(inputs, planOut, p, *overwrite)
	if err == nil && subdirs != nil {
		err = mirror(planned, subdirs, *out)
	}
	if err != nil {
		return usageErr("convert: %v", err)
	}
	if *dryRun {
		limits, err := a.limits()
		if err != nil {
			return err
		}
		return a.printDryRun(planned, nil, jobs, limits, false)
	}
	if _, err := tools.Path("ffmpeg"); err != nil {
		return err
	}
	return a.runQueue(ctx, planned, jobs, *failFast)
}

func parseJobs(verb, value string) (int, error) {
	if value == "auto" {
		return 0, nil
	}
	jobs, err := strconv.Atoi(value)
	if err != nil || jobs < 1 {
		return 0, usageErr("%s: --jobs must be auto or a positive number, got %q", verb, value)
	}
	return jobs, nil
}

// expandInputs lists the media files of paths. A directory input always means a
// directory output. Recursive takes a directory's whole tree; when that meets an
// output directory, subdirs holds each file's folder relative to its input, for
// mirror, and is nil otherwise.
func expandInputs(paths []string, out *string, recursive bool) (inputs, subdirs []string, err error) {
	tree := false
	for _, path := range paths {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, nil, err
		}
		found := []string{path}
		switch {
		case !fi.IsDir():
		case recursive:
			found, err = walkMedia(path, *out)
		default:
			found, err = mediaPaths(path)
		}
		if err != nil {
			return nil, nil, err
		}
		if fi.IsDir() && *out != "" {
			*out = filepath.Clean(*out) + string(filepath.Separator)
			tree = tree || recursive
		}
		for _, file := range found {
			sub := "."
			if fi.IsDir() {
				sub, _ = filepath.Rel(path, filepath.Dir(file))
			}
			subdirs = append(subdirs, sub)
		}
		inputs = append(inputs, found...)
	}
	if len(inputs) == 0 {
		return nil, nil, errors.New("no media files found in the given inputs")
	}
	if !tree {
		subdirs = nil
	}
	return inputs, subdirs, nil
}

// walkMedia lists the media files in the tree of root. It skips hidden folders,
// converted folders of earlier runs and the output folder, and does not follow
// links to folders.
func walkMedia(root, out string) ([]string, error) {
	skip, _ := filepath.Abs(out)
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if abs, _ := filepath.Abs(path); path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "converted" || out != "" && abs == skip) {
			return filepath.SkipDir
		}
		found, err := mediaPaths(path)
		paths = append(paths, found...)
		return err
	})
	return paths, err
}

// mirror moves outputs planned next to their inputs (with an empty out) under
// out, each in its folder from subdirs, so a tree keeps its shape.
func mirror(jobs []convert.Job, subdirs []string, out string) error {
	seen := map[string]string{}
	for i := range jobs {
		jobs[i].Output = filepath.Join(out, subdirs[i], filepath.Base(jobs[i].Output))
		if prev, ok := seen[jobs[i].Output]; ok {
			return fmt.Errorf("%s and %s would both be written to %s", prev, jobs[i].Input, jobs[i].Output)
		}
		seen[jobs[i].Output] = jobs[i].Input
	}
	return nil
}

func (a *app) limits() (map[string]int, error) {
	path, err := configPath("config.yml")
	if err != nil {
		return nil, err
	}
	return convert.LoadLimits(path)
}

// printDryRun shows the jobs without running them: the plan of each file when
// there is one, and the final commands when there is none or with explain.
func (a *app) printDryRun(planned []convert.Job, plans map[int]*convert.FilePlan, jobs int, limits map[string]int, explain bool) error {
	var doc struct {
		SchemaVersion int         `json:"schemaVersion"`
		Jobs          []dryRunJob `json:"jobs"`
	}
	doc.SchemaVersion = schemaVersion
	for _, job := range planned {
		p := job.Preset
		limit := jobs
		if limit == 0 {
			limit = limits[p.Load]
		}
		threads := max(a.cores/limit, 1)
		cmd, err := convert.BuildArgs(p, job.Input, job.Output, threads)
		if err != nil {
			return usageErr("convert: %v", err)
		}
		cmd = append([]string{"ffmpeg"}, cmd...)
		exif, err := convert.ExifArgs(p.Exif, job.Input, job.Output)
		if err != nil {
			return err
		}
		if exif != nil {
			exif = append([]string{"exiftool"}, exif...)
		}
		doc.Jobs = append(doc.Jobs, dryRunJob{job, p.Name, p.Load, threads, cmd, exif, plans[job.ID]})
		if a.json {
			continue
		}
		if plan := plans[job.ID]; plan != nil {
			printPlan(a.stdout, job, plan)
		}
		if plans == nil || explain {
			fmt.Fprintln(a.stdout, shellJoin(cmd))
			if exif != nil {
				fmt.Fprintln(a.stdout, shellJoin(exif))
			}
		}
	}
	if a.json {
		return a.writeJSON(doc)
	}
	return nil
}

// runQueue runs the jobs of any command and sums up their results.
func (a *app) runQueue(ctx context.Context, planned []convert.Job, jobs int, failFast bool) error {
	limits, err := a.limits()
	if err != nil {
		return err
	}
	q := &convert.Queue{Limits: limits, Cores: a.cores, FailFast: failFast, OnEvent: a.convertEvents(len(planned))}
	q.SetJobs(jobs)
	errs := q.Run(ctx, planned)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if len(errs) == 1 && errs[0] != nil {
		if errors.Is(errs[0], fsutil.ErrExists) {
			return fmt.Errorf("%w; pass --overwrite to replace it", errs[0])
		}
		return errs[0]
	}
	return batchErr("convert", failed, len(errs))
}

// convertEvents returns the event sink: JSON Lines on stdout with --json,
// otherwise a progress view on stderr.
func (a *app) convertEvents(total int) func(convert.Event) {
	if a.json {
		enc := json.NewEncoder(a.stdout)
		return func(ev convert.Event) {
			enc.Encode(struct {
				SchemaVersion int `json:"schemaVersion"`
				convert.Event
			}{schemaVersion, ev})
		}
	}
	live := false
	if f, ok := a.stderr.(*os.File); ok {
		live = term.IsTerminal(int(f.Fd()))
	}
	v := &progressView{w: a.stderr, live: live, total: total, running: map[int]convert.Event{}, sizes: map[int]int64{}}
	return v.handle
}

// progressView prints conversion events to stderr: on a terminal a live block with
// one line per running job; otherwise only finished jobs.
type progressView struct {
	mu                  sync.Mutex
	w                   io.Writer
	live                bool
	total, done, failed int
	running             map[int]convert.Event
	sizes               map[int]int64 // input size per job, read when it starts
	drawn               int
}

func (v *progressView) handle(ev convert.Event) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.drawn > 0 {
		fmt.Fprintf(v.w, "\x1b[%dA\r\x1b[J", v.drawn) // back to the top of the live block
		v.drawn = 0
	}
	name := safeText(filepath.Base(ev.Input))
	switch ev.Type {
	case "start", "progress":
		if ev.Type == "start" {
			if fi, err := os.Stat(ev.Input); err == nil {
				v.sizes[ev.JobID] = fi.Size() // before an in-place job replaces it
			}
		}
		v.running[ev.JobID] = ev
	case "done":
		delete(v.running, ev.JobID)
		v.done++
		sizes := ""
		if fi, err := os.Stat(ev.Output); err == nil && v.sizes[ev.JobID] > 0 {
			sizes = fmt.Sprintf("  %s%s → %s%s", anGray, fmtSize(v.sizes[ev.JobID]), fmtSize(fi.Size()), anReset)
		}
		fmt.Fprintf(v.w, "  %s%s%s → %s%s\n", anGreen, name, anReset, safeText(ev.Output), sizes)
	case "skipped":
		delete(v.running, ev.JobID)
		v.done++
		fmt.Fprintf(v.w, "  %s%s: %s%s\n", anGray, name, safeText(ev.Error), anReset)
	case "failed", "canceled":
		delete(v.running, ev.JobID)
		v.failed++
		// A single job's error is reported once, as the command error.
		if ev.Error != "not started" && v.total > 1 {
			msg := ev.Error
			if ev.Type == "canceled" {
				msg = "canceled"
			}
			fmt.Fprintf(v.w, "  %s%s: %s%s\n", anRed, name, safeText(msg), anReset)
		}
	}
	if !v.live || len(v.running) == 0 {
		return
	}

	width := 80
	if f, ok := v.w.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			width = w
		}
	}
	ids := make([]int, 0, len(v.running))
	for id := range v.running {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		fmt.Fprintln(v.w, visTrunc(progressLine(v.running[id]), width-1))
	}
	fmt.Fprintln(v.w, visTrunc(fmt.Sprintf("  %s%d/%d done, %d failed, %d running%s", anGray, v.done, v.total, v.failed, len(v.running), anReset), width-1))
	v.drawn = len(ids) + 1
}

func progressLine(ev convert.Event) string {
	line := fmt.Sprintf("  %s[%d]%s %s", anMagenta, ev.JobID, anReset, safeText(filepath.Base(ev.Input)))
	p := ev.Progress
	if p == nil {
		return line + anGray + "  starting…" + anReset
	}
	if p.DurationMs > 0 {
		filled := int(p.Percent / 5)
		line += fmt.Sprintf("  %s%s%s%s %5.1f%%", anTeal, strings.Repeat("█", filled), anDim+strings.Repeat("░", 20-filled), anReset, p.Percent)
	} else {
		line += "  " + fmtDurMs(p.OutTimeMs)
	}
	if p.Speed > 0 {
		line += fmt.Sprintf("  %s%.2fx%s", anGray, p.Speed, anReset)
	}
	return line
}

// shellJoin quotes args for copy-pasting into a POSIX shell.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, s := range args {
		safe := s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_./:=,+@%", r)
		}) < 0
		if safe {
			quoted[i] = s
		} else {
			quoted[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
