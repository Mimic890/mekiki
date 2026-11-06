package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Mimic890/mekiki/pkg/convert"
	"github.com/Mimic890/mekiki/pkg/probe"
	"github.com/Mimic890/mekiki/pkg/tools"
)

const tokensHelp = `

Everything is kept; only what a token names changes.

Tokens:
  h265 h264 av1            video codec
  webp jpeg avif png       photo format
  opus aac mp3 flac        music format
  q21 good best            quality, in the encoder's own scale
  1080p 50% 2000px         size; only a percentage can enlarge
  30fps                    frame rate
  gpu                      hardware encoder
  a:opus a:192k            audio codec or bitrate, all tracks
  a:rus=opus a:rus=192k    the same for one language
  160k                     music bitrate
  -a:eng                   remove the tracks of a language
  -s                       remove all subtitles
  -exif                    remove metadata (music: also the cover)
  NAME                     a preset saved with --save; manual tokens win over it

Without -o, outputs go into a "converted" directory next to each input.`

var verbMedia = map[string]string{"video": convert.MediaVideo, "photo": convert.MediaPhoto, "music": convert.MediaMusic}

// shrinkTokens is the choice mekiki shrink makes for each kind of file.
var shrinkTokens = map[string][]string{convert.MediaVideo: {"h265"}, convert.MediaPhoto: {"webp"}, convert.MediaMusic: {"opus"}}

// verb runs video, photo, music and shrink: tokens become a plan for each file, for the same queue as convert.
func (a *app) verb(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	out := fs.String("o", "", "output file, or directory for several inputs")
	overwrite := fs.Bool("overwrite", false, "replace existing output files, including the input itself")
	explain := fs.Bool("explain", false, "print the plan and the final commands without running them")
	dryRun := fs.Bool("dry-run", false, "print the plan without running it")
	failFast := fs.Bool("fail-fast", false, "stop all jobs after the first failure")
	jobsFlag := "auto"
	fs.StringVar(&jobsFlag, "jobs", "auto", "parallel jobs: a number or auto (per load class, see config.yml)")
	fs.StringVar(&jobsFlag, "j", "auto", "shorthand for --jobs")
	usage := "shrink [flags] <file|directory...>\n\nMakes files smaller with safe defaults and keeps everything else.\nA result that is not smaller is thrown away."
	var save *string
	var yes *bool
	if verb != "shrink" {
		save = fs.String("save", "", "save the tokens as a preset with this name")
		yes = fs.Bool("yes", false, "replace an existing user preset with --save")
		usage = verb + " [flags] <file|directory...> <tokens...>" + tokensHelp
	}

	tokens, rest := splitDashTokens(fs, args)
	positional, err := a.parseFlags(fs, rest, usage)
	if err != nil {
		return err
	}
	jobs, err := parseJobs(verb, jobsFlag)
	if err != nil {
		return err
	}
	lib, err := loadLibrary()
	if err != nil {
		return err
	}

	var paths []string
	for _, arg := range positional {
		_, presetErr := lib.Get(arg)
		_, statErr := os.Stat(arg)
		if convert.IsToken(arg) || presetErr == nil || statErr != nil {
			tokens = append(tokens, arg)
		} else {
			paths = append(paths, arg)
		}
	}
	if verb == "shrink" && len(tokens) > 0 {
		return usageErr("shrink: takes only existing files and directories, got %s; for tokens use mekiki video, photo or music", tokens[0])
	}

	var spec convert.Spec
	if verb != "shrink" {
		if spec, err = lib.ParseSpec(verbMedia[verb], tokens); err != nil {
			return usageErr("%s: %v", verb, err)
		}
		if len(spec) == 0 {
			return usageErr("%s: name at least one change, see mekiki %s -h; mekiki shrink needs no tokens", verb, verb)
		}
		if *save != "" {
			if err := a.saveTokens(lib, *save, verbMedia[verb], spec, *yes); err != nil {
				return err
			}
			if len(paths) == 0 {
				return nil
			}
		}
	}
	if len(paths) == 0 {
		return usageErr("%s: need at least one input file or directory", verb)
	}

	inputs, err := expandInputs(paths, out)
	if err != nil {
		return err
	}
	if _, err := tools.Path("ffprobe"); err != nil {
		return err
	}
	if _, err := tools.Path("ffmpeg"); err != nil {
		return err
	}
	planned, plans, err := a.planFiles(ctx, verb, lib, spec, inputs, *out, *overwrite)
	if err != nil {
		return err
	}

	if *dryRun || *explain {
		limits, err := a.limits()
		if err != nil {
			return err
		}
		return a.printDryRun(planned, plans, jobs, limits, *explain)
	}
	if !a.json {
		for _, job := range planned {
			printPlan(a.stderr, job, plans[job.ID])
		}
	}
	for _, job := range planned {
		if job.Preset.Exif != "" {
			if _, err := tools.Path("exiftool"); err != nil {
				return fmt.Errorf("%w; photos keep or clean their EXIF with it", err)
			}
			break
		}
	}
	return a.runQueue(ctx, planned, jobs, *failFast)
}

// planFiles probes the inputs and composes one job and plan per file. Files that need a decision stop
// the whole run, since the user can answer them all at once; shrink has nobody to ask, so it skips them.
func (a *app) planFiles(ctx context.Context, verb string, lib *convert.Library, spec convert.Spec, inputs []string, out string, overwrite bool) ([]convert.Job, map[int]*convert.FilePlan, error) {
	infos := make([]*probe.MediaInfo, len(inputs))
	specs := make([]convert.Spec, len(inputs))
	media := make([]string, len(inputs))
	containers := make([]string, len(inputs))
	failures := map[int]string{}
	for i, input := range inputs {
		pctx, cancel := context.WithTimeout(ctx, probe.DefaultTimeout)
		info, err := probe.ScanFileContext(pctx, input)
		cancel()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil {
			failures[i] = err.Error()
			continue
		}
		infos[i], specs[i], media[i] = info, spec, verbMedia[verb]
		if verb == "shrink" {
			media[i] = convert.Kind(info)
			if tokens := shrinkTokens[media[i]]; tokens != nil {
				if specs[i], err = lib.ParseSpec(media[i], tokens); err != nil {
					return nil, nil, err
				}
			}
		}
		containers[i] = lib.ContainerFor(media[i], specs[i])
	}

	planned, err := convert.PlanContainers(inputs, out, containers, overwrite)
	if err != nil {
		return nil, nil, usageErr("%s: %v", verb, err)
	}
	plans := map[int]*convert.FilePlan{}
	var kept []convert.Job
	var problems []string
	for i, job := range planned {
		var plan *convert.FilePlan
		switch {
		case failures[i] != "":
			plan = &convert.FilePlan{Problems: []string{failures[i]}}
		case verb == "shrink" && media[i] == "":
			plan = &convert.FilePlan{Problems: []string{"has no streams mekiki can convert"}}
		default:
			job.Preset, plan = lib.Compose(ctx, media[i], specs[i], infos[i], job.Output)
			job.OnlySmaller = verb == "shrink"
		}
		plans[job.ID] = plan
		if verb == "shrink" && len(plan.Problems) > 0 {
			if !a.json {
				fmt.Fprintf(a.stderr, "  %sskipping %s: %s%s\n", anYellow, safeText(job.Input), safeText(plan.Problems[0]), anReset)
			}
			continue
		}
		for _, problem := range plan.Problems {
			problems = append(problems, filepath.Base(job.Input)+": "+problem)
		}
		kept = append(kept, job)
	}
	if len(problems) > 0 {
		if !a.json {
			for _, job := range kept {
				if len(plans[job.ID].Lines) > 0 {
					printPlan(a.stderr, job, plans[job.ID])
				}
			}
		}
		if a.json {
			return nil, nil, usageErr("nothing was converted: %s", strings.Join(problems, "; "))
		}
		for _, problem := range problems {
			fmt.Fprintf(a.stderr, "%s%s%s\n", anRed, safeText(problem), anReset)
		}
		return nil, nil, usageErr("nothing was converted")
	}
	if len(kept) == 0 {
		return nil, nil, errors.New("no file can be converted")
	}
	return kept, plans, nil
}

func (a *app) saveTokens(lib *convert.Library, name, media string, spec convert.Spec, replace bool) error {
	if convert.IsToken(name) || lib.IsBuiltin(name) {
		return usageErr("--save: %s is already a token or built-in preset, pick another name", name)
	}
	if existing, err := lib.Get(name); err == nil && existing.Source == "user" && !replace {
		return usageErr("--save: user preset %s exists; pass --yes to replace it", name)
	}
	tokens := spec.Tokens()
	err := lib.Save(convert.Preset{Name: name, Media: media, Description: strings.Join(tokens, " "), Tokens: tokens})
	if err != nil {
		return usageErr("--save: %v", err)
	}
	if !a.json {
		fmt.Fprintf(a.stderr, "saved preset %s: %s\n", name, strings.Join(tokens, " "))
	}
	return nil
}

// splitDashTokens takes the arguments that start with "-" but are not flags
// (-a:eng, -s, -exif and mistyped ones) out of args, up to a "--".
func splitDashTokens(fs *flag.FlagSet, args []string) (tokens, rest []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return tokens, append(rest, args[i:]...)
		}
		if !strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		switch {
		case name == "h" || name == "help" || name == "json":
			rest = append(rest, arg)
		case f == nil:
			tokens = append(tokens, arg)
		default:
			rest = append(rest, arg)
			b, isBool := f.Value.(interface{ IsBoolFlag() bool })
			if !hasValue && !(isBool && b.IsBoolFlag()) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		}
	}
	return tokens, rest
}

// printPlan shows what changes in one file, as a diff against the source.
func printPlan(w io.Writer, job convert.Job, plan *convert.FilePlan) {
	size := ""
	if fi, err := os.Stat(job.Input); err == nil {
		size = "  " + fmtSize(fi.Size())
	}
	fmt.Fprintf(w, "%s%s%s%s  %s→ %s%s\n", anBold, safeText(filepath.Base(job.Input)), anReset, size, anGray, safeText(job.Output), anReset)
	for _, line := range plan.Lines {
		fmt.Fprintf(w, "  %s%-5s%s  %s\n", anTeal, line.Kind, anReset, safeText(line.Text))
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(w, "  %swarn   %s%s\n", anYellow, safeText(warning), anReset)
	}
}
