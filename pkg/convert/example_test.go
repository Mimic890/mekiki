package convert_test

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"strings"

	"github.com/Mimic890/mekiki/pkg/convert"
	"github.com/Mimic890/mekiki/pkg/probe"
)

// The same steps as `mekiki video film.mkv h265 q21 a:rus=opus -a:eng`:
// parse the tokens, plan the file, then run the job.
func Example() {
	ctx := context.Background()
	lib, err := convert.LoadLibrary("") // built-in presets only
	if err != nil {
		log.Fatal(err)
	}
	spec, err := lib.ParseSpec(convert.MediaVideo, []string{"h265", "q21", "a:rus=opus", "-a:eng"})
	if err != nil {
		log.Fatal(err) // an unknown or conflicting token, with a hint
	}
	info, err := probe.ScanFileContext(ctx, "film.mkv")
	if err != nil {
		log.Fatal(err)
	}
	// "out/" is a directory; "" would write next to the input, into converted/.
	jobs, err := convert.PlanContainers([]string{"film.mkv"}, "out/", []string{lib.ContainerFor(convert.MediaVideo, spec)}, false)
	if err != nil {
		log.Fatal(err)
	}
	job := jobs[0]
	var plan *convert.FilePlan
	job.Preset, plan = lib.Compose(ctx, convert.MediaVideo, spec, info, job.Output)
	if len(plan.Problems) > 0 {
		log.Fatal(strings.Join(plan.Problems, "; ")) // a decision only the user can make
	}
	for _, line := range plan.Lines {
		fmt.Println(line.Kind, line.Text)
	}
	err = convert.Run(ctx, job, 0, func(p convert.Progress) {
		fmt.Printf("\r%.0f%%", p.Percent)
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleLibrary_ParseSpec() {
	lib, err := convert.LoadLibrary("")
	if err != nil {
		log.Fatal(err)
	}
	spec, err := lib.ParseSpec(convert.MediaVideo, []string{"h265", "q21", "a:rus=opus", "-a:eng"})
	fmt.Println(spec.Tokens(), err)

	_, err = lib.ParseSpec(convert.MediaVideo, []string{"h256"})
	fmt.Println(err)
	_, err = lib.ParseSpec(convert.MediaVideo, []string{"h265", "av1"})
	fmt.Println(err)
	// Output:
	// [-a:eng a:rus=opus q21 h265] <nil>
	// unknown token h256: did you mean h264 or h265?
	// h265 and av1 conflict: both set the same thing, keep one
}

func ExampleBuildArgs() {
	lib, err := convert.LoadLibrary("")
	if err != nil {
		log.Fatal(err)
	}
	p, err := lib.Get("hevc")
	if err != nil {
		log.Fatal(err)
	}
	args, err := convert.BuildArgs(p, "in.mkv", "out.mkv", 0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("ffmpeg", strings.Join(args, " "))
	// Output:
	// ffmpeg -hide_banner -nostdin -y -v error -progress pipe:1 -nostats -i in.mkv -map 0 -c copy -c:V libx265 -crf:V 22 -preset:V medium -c:a copy out.mkv
}

// A batch with a built-in preset. In auto mode each load class (CPU, GPU,
// light, I/O) has its own concurrency limit.
func ExampleQueue() {
	lib, err := convert.LoadLibrary("")
	if err != nil {
		log.Fatal(err)
	}
	p, err := lib.Get("hevc")
	if err != nil {
		log.Fatal(err)
	}
	jobs, err := convert.Plan([]string{"a.mkv", "b.mkv", "c.mkv"}, "out/", p, false)
	if err != nil {
		log.Fatal(err)
	}
	q := &convert.Queue{
		Limits:  convert.DefaultLimits(),
		Cores:   runtime.NumCPU(),
		OnEvent: func(e convert.Event) { fmt.Println(e.Type, e.Input, e.Error) },
	}
	for i, err := range q.Run(context.Background(), jobs) {
		if err != nil {
			fmt.Println(jobs[i].Input, "failed:", err)
		}
	}
}
