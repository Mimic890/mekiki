package convert

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Mimic890/mekiki/pkg/probe"
)

func TestParseSpec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.yml")
	lib, err := LoadLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Save(Preset{Name: "myhevc", Media: MediaVideo, Tokens: []string{"h265", "q21", "a:opus", "-a:eng"}}); err != nil {
		t.Fatal(err)
	}

	spec, err := lib.ParseSpec(MediaVideo, []string{"myhevc", "a:192k", "q18", "a:eng=aac"})
	want := Spec{"v": "h265", "q": "q18", "a": "a:opus", "a.br": "a:192k", "a:eng": "a:eng=aac"}
	if err != nil || !reflect.DeepEqual(spec, want) {
		t.Fatalf("manual tokens must win over the preset: %v, %v", spec, err)
	}
	if got := spec.Tokens(); !reflect.DeepEqual(got, []string{"a:opus", "a:192k", "a:eng=aac", "q18", "h265"}) {
		t.Fatalf("Tokens() = %v", got)
	}

	for _, test := range []struct {
		media string
		spec  string
		want  string
	}{
		{MediaVideo, "h265 av1", "conflict"},
		{MediaVideo, "h265 hevc", "conflict"},
		{MediaVideo, "q21 best", "conflict"},
		{MediaVideo, "1080p 50%", "conflict"},
		{MediaVideo, "a:opus a:aac", "conflict"},
		{MediaVideo, "-a:eng a:eng=opus", "conflict"},
		{MediaVideo, "-a:eng a:eng=192k", "conflict"},
		{MediaVideo, "a:copy a:192k", "conflict"},
		{MediaMusic, "q2 160k", "conflict"},
		{MediaVideo, "opus", "write a:opus"},
		{MediaVideo, "160k", "write a:160k"},
		{MediaVideo, "webp", "does not apply"},
		{MediaPhoto, "h265", "does not apply"},
		{MediaPhoto, "30fps", "does not apply"},
		{MediaPhoto, "a:opus", "does not apply"},
		{MediaMusic, "-s", "does not apply"},
		{MediaVideo, "h256", "did you mean h264 or h265?"},
		{MediaVideo, "1080", "bare number"},
		{MediaVideo, "-z", "did you mean -s?"},
		{MediaVideo, "-bogus", "removal tokens"},
		{MediaVideo, "a:wav", "unknown audio codec"},
		{MediaVideo, "a:RUS=opus", "cannot read"},
		{MediaVideo, "film.mkv", "no such file"},
		{MediaVideo, "remux", "convert --preset remux"},
		{MediaPhoto, "myhevc", "is for video files"},
		{MediaVideo, "0p", "neither a file"},
	} {
		_, err := lib.ParseSpec(test.media, strings.Fields(test.spec))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want %q", test.spec, err, test.want)
		}
	}
	for _, arg := range []string{"h265", "webp", "opus", "q21", "good", "1080p", "50%", "2000px", "30fps", "gpu", "a:wav", "-s", "-exif", "160k"} {
		if !IsToken(arg) {
			t.Errorf("IsToken(%s) = false", arg)
		}
	}
	for _, arg := range []string{"film.mkv", "./h265", "trip", "myhevc"} {
		if IsToken(arg) {
			t.Errorf("IsToken(%s) = true", arg)
		}
	}
}

func TestFitSize(t *testing.T) {
	for _, test := range []struct {
		tok                string
		w, h, wantW, wantH int
	}{
		{"", 1920, 1080, 1920, 1080},
		{"1080p", 3840, 2160, 1920, 1080},
		{"1080p", 3840, 1600, 1920, 800},   // scope film: the width limits it
		{"1080p", 2160, 3840, 1080, 1920},  // portrait
		{"1080p", 1280, 720, 1280, 720},    // never enlarged
		{"720p", 1440, 1080, 960, 720},     // 4:3: the height limits it
		{"2000px", 4000, 3000, 2000, 1500}, // longest side
		{"2000px", 3000, 4000, 1500, 2000},
		{"2000px", 800, 600, 800, 600},
		{"50%", 1921, 1081, 960, 540}, // always even
		{"200%", 100, 50, 200, 100},
		{"1%", 10, 10, 2, 2},
	} {
		if w, h := fitSize(test.tok, test.w, test.h); w != test.wantW || h != test.wantH {
			t.Errorf("fitSize(%s, %d, %d) = %d, %d", test.tok, test.w, test.h, w, h)
		}
	}
}

func TestMapArgRemoves(t *testing.T) {
	for sel, want := range map[string]string{"-a:1": "-0:a:1", "-s": "-0:s", "-v": "-0:v", "-t:0": "-0:t:0"} {
		if got, err := mapArg(sel); err != nil || got != want {
			t.Errorf("mapArg(%q) = %q, %v; want %q", sel, got, err, want)
		}
	}
	for _, bad := range []string{"-3", "-all", "--a", "-q"} {
		if _, err := mapArg(bad); err == nil {
			t.Errorf("mapArg(%q) must fail", bad)
		}
	}
}

func film() *probe.MediaInfo {
	return &probe.MediaInfo{
		FilePath: "/media/film.mkv",
		Video: []probe.VideoStream{
			{Codec: "hevc", Width: 1920, Height: 1080, FPS: 23.976, PixFmt: "yuv420p", BitDepth: 8},
			{Index: 6, Codec: "mjpeg", Width: 300, Height: 300, IsAttachedPic: true},
		},
		Audio: []probe.AudioStream{
			{Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", Language: "rus"},
			{Codec: "dts", Channels: 6, ChannelLayout: "5.1", Language: "eng"},
			{Codec: "opus", Channels: 2, ChannelLayout: "stereo", Language: "jpn"},
		},
		Subtitle: []probe.SubStream{{Codec: "subrip", Language: "rus"}, {Codec: "subrip", Language: "eng"}},
	}
}

// compose parses tokens and composes a plan, skipping the test without ffmpeg.
func compose(t *testing.T, media, tokens string, info *probe.MediaInfo, output string) (Preset, *FilePlan) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	lib, err := LoadLibrary("")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := lib.ParseSpec(media, strings.Fields(tokens))
	if err != nil {
		t.Fatal(err)
	}
	return lib.Compose(context.Background(), media, spec, info, output)
}

// One Library serves concurrent Compose calls; the gpu token shares a cache. Run with -race.
func TestComposeConcurrent(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	lib, _ := LoadLibrary("")
	spec, err := lib.ParseSpec(MediaVideo, []string{"h265", "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, plan := lib.Compose(canceled, MediaVideo, spec, film(), "film.mkv"); len(plan.Problems) != 1 {
		t.Fatalf("canceled: %v", plan.Problems)
	}
	if _, cached := lib.gpu["hevc"]; cached {
		t.Fatal("a canceled test encode must not be cached")
	}

	results := make([]string, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			p, plan := lib.Compose(context.Background(), MediaVideo, spec, film(), "film.mkv")
			results[i] = p.Video.Codec + strings.Join(plan.Problems, "; ")
		})
	}
	wg.Wait()
	for _, r := range results {
		if r != results[0] {
			t.Fatalf("results differ: %q", results)
		}
	}
}

func TestCoverAttachments(t *testing.T) {
	info := &probe.MediaInfo{Video: []probe.VideoStream{
		{Index: 0, Codec: "png", IsAttachedPic: true, Tags: map[string]string{"FILENAME": "front.png", "MIMETYPE": "image/png"}},
		{Index: 1, Codec: "h264"},
		{Index: 2, Codec: "mjpeg", IsAttachedPic: true, Tags: map[string]string{"filename": "a:b.jpg"}},
	}}
	got := attachments(info, nil, "out.mkv")
	want := []attachment{{0, "front.png", "image/png", "front.png"}, {2, "cover.jpg", "image/jpeg", "cover.jpg"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attachments = %+v", got)
	}
	if attachments(info, nil, "out.mp4") != nil || len(attachments(info, []string{"v:1", "a"}, "out.mkv")) != 0 {
		t.Fatal("only Matroska outputs attach, and only covers the selectors keep")
	}
	for _, test := range []struct {
		tracks []string
		want   bool
	}{
		{nil, true}, {[]string{"all"}, true}, {[]string{"all", "-v"}, false}, {[]string{"all", "-a:0"}, true},
		{[]string{"v"}, true}, {[]string{"v:2"}, true}, {[]string{"v:0"}, false}, {[]string{"3"}, true}, {[]string{"a"}, false},
	} {
		if got := kept(test.tracks, 3, "v", 2); got != test.want {
			t.Errorf("kept(%v) = %v", test.tracks, got)
		}
	}
}

func TestComposeVideo(t *testing.T) {
	p, plan := compose(t, MediaVideo, "h265 q21 a:rus=opus -a:eng", film(), "out/film.mkv")
	if len(plan.Problems) > 0 {
		t.Fatal(plan.Problems)
	}
	wantLines := []PlanLine{
		{"video", "hevc→h265 q21   1920x1080 23.976: as is"},
		{"audio", "rus ac3 5.1 → opus 5.1 · eng dts 5.1 → remove · jpn opus stereo: as is"},
		{"subs", "rus, eng: as is"},
	}
	if !reflect.DeepEqual(plan.Lines, wantLines) {
		t.Fatalf("lines = %+v", plan.Lines)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	args, err := BuildArgs(p, "in.mkv", "out.mkv", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The cover leaves the mapping and comes back as a Matroska attachment.
	want := "-i in.mkv -map 0 -map -0:a:1 -map -0:6 -c copy -c:v:0 libx265 -crf:v:0 21 -preset:v:0 medium -c:a:0 libopus -filter:a:0 channelmap=channel_layout=5.1 " +
		"-attach cover.jpg -metadata:s:t:m:filename:cover.jpg mimetype=image/jpeg out.mkv"
	if got := strings.Join(args[8:], " "); got != want {
		t.Fatalf("args:\n got %s\nwant %s", got, want)
	}
	if p.Load != LoadCPUHeavy {
		t.Fatalf("load = %s", p.Load)
	}

	// Only what is named changes: the video is not encoded for an audio token.
	p, plan = compose(t, MediaVideo, "a:192k -s -exif", film(), "film.mkv")
	if p.Video.Codec != "" || p.Load != LoadLight || !strings.HasSuffix(plan.Lines[0].Text, ": as is") {
		t.Fatalf("video must be copied: %+v %+v", p, plan.Lines)
	}
	if !slices.Contains(p.Tracks, "-s") || !slices.Contains(p.Args, "-map_metadata:g") {
		t.Fatalf("preset = %+v", p)
	}
	if len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "dts has no default encoder") {
		t.Fatalf("a bitrate alone needs an encoder for dts: %v", plan.Problems)
	}

	// A size that changes nothing does not cause an encode.
	if p, _ := compose(t, MediaVideo, "4320p", film(), "film.mkv"); p.Video.Codec != "" {
		t.Fatalf("encoded for no reason: %+v", p.Video)
	}
	if p, plan := compose(t, MediaVideo, "720p", film(), "film.mkv"); p.Video.Codec != "libx265" || !strings.Contains(plan.Lines[0].Text, "1920x1080→1280x720") || !slices.Contains(p.Args, "scale=1280:720") {
		t.Fatalf("source codec must be kept for a resize: %+v %+v", p, plan.Lines)
	}
	if _, plan := compose(t, MediaVideo, "a:rus=aac a:deu=aac", film(), "film.mkv"); !strings.Contains(strings.Join(plan.Warnings, ""), "no deu audio track") {
		t.Fatalf("missing language must warn: %v", plan.Warnings)
	}
	if _, plan := compose(t, MediaVideo, "q99", film(), "film.mkv"); len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "0..51") {
		t.Fatalf("range: %v", plan.Problems)
	}

	// The first audio track, ten-bit video and a rotated phone clip.
	info := film()
	info.Video[0].PixFmt, info.Video[0].BitDepth, info.Video[0].Rotation = "yuv420p10le", 10, 90
	p, plan = compose(t, MediaVideo, "h264 50%", info, "film.mkv")
	if p.Video.PixFmt != "yuv420p10le" || !slices.Contains(p.Args, "scale=540:960") || !strings.Contains(plan.Lines[0].Text, "10bit") {
		t.Fatalf("depth and rotation: %+v %+v", p, plan.Lines)
	}
	info.Video[0].HDR = probe.HDRInfo{IsDolbyVision: true}
	if _, plan := compose(t, MediaVideo, "h265", info, "film.mkv"); len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "Dolby Vision") {
		t.Fatalf("dv: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaVideo, "a:aac", info, "film.mkv"); len(plan.Problems) != 0 {
		t.Fatalf("copying Dolby Vision is fine: %v", plan.Problems)
	}

	// Cover art before the main video leaves the video streams only where it is attached again.
	info = film()
	info.Video[0].Index, info.Video[1].Index = 1, 0
	info.Video[0], info.Video[1] = info.Video[1], info.Video[0]
	if p, _ := compose(t, MediaVideo, "h264", info, "film.mkv"); p.VideoStream != "v:0" {
		t.Fatalf("mkv: %s", p.VideoStream)
	}
	if p, _ := compose(t, MediaVideo, "h264", info, "film.mp4"); p.VideoStream != "v:1" {
		t.Fatalf("mp4: %s", p.VideoStream)
	}
}

func TestComposeContainers(t *testing.T) {
	info := film()
	info.Audio[0].Codec = "flac"
	info.Audio = info.Audio[:1]
	_, plan := compose(t, MediaVideo, "h265 -s", info, "film.mp4")
	if len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "flac does not fit in mp4: replace it with a:aac") {
		t.Fatalf("copied flac in mp4: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaVideo, "a:flac -s", film(), "film.mp4"); !strings.Contains(strings.Join(plan.Problems, ""), "flac does not fit") {
		t.Fatalf("named flac in mp4: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaVideo, "a:aac -s", film(), "film.mp4"); len(plan.Problems) != 0 {
		t.Fatalf("the replacement must be accepted: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaVideo, "a:aac", film(), "film.mp4"); !strings.Contains(strings.Join(plan.Problems, ""), "subrip subtitles do not fit") {
		t.Fatalf("subtitles: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaVideo, "a:opus -s", film(), "film.webm"); !strings.Contains(strings.Join(plan.Problems, ""), "video hevc does not fit") {
		t.Fatalf("webm: %v", plan.Problems)
	}
}

func TestComposePhotoAndMusic(t *testing.T) {
	jpeg := &probe.MediaInfo{FilePath: "/media/a.jpg", Video: []probe.VideoStream{{Codec: "mjpeg", Width: 4000, Height: 3000, PixFmt: "yuvj420p"}}}
	p, plan := compose(t, MediaPhoto, "webp 2000px q85 -exif", jpeg, "a.webp")
	if len(plan.Problems) > 0 || p.Exif != ExifStrip || p.Video.Codec != "libwebp" || *p.Video.CRF != 85 || !slices.Contains(p.Args, "scale=2000:1500") || !slices.Contains(p.InputArgs, "-noautorotate") {
		t.Fatalf("photo: %+v %+v", p, plan)
	}
	if p, plan := compose(t, MediaPhoto, "2000px", jpeg, "a.jpg"); p.Exif != ExifCopy || p.Video.Codec != "mjpeg" || len(plan.Problems) > 0 {
		t.Fatalf("source format must be kept: %+v %+v", p, plan)
	}
	if p, _ := compose(t, MediaPhoto, "-exif", jpeg, "a.jpg"); p.Video.Codec != "copy" {
		t.Fatalf("removing tags must not re-encode: %+v", p.Video)
	}
	if _, plan := compose(t, MediaPhoto, "q85", jpeg, "a.jpg"); !strings.Contains(strings.Join(plan.Problems, ""), "1..31") {
		t.Fatalf("jpeg scale: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaPhoto, "webp", jpeg, "a.jpg"); !strings.Contains(strings.Join(plan.Problems, ""), "does not match") {
		t.Fatalf("output name: %v", plan.Problems)
	}
	if _, plan := compose(t, MediaMusic, "opus", jpeg, "a.opus"); !strings.Contains(strings.Join(plan.Problems, ""), "use mekiki photo") {
		t.Fatalf("wrong verb: %v", plan.Problems)
	}

	song := &probe.MediaInfo{FilePath: "/media/a.flac",
		Audio: []probe.AudioStream{{Codec: "flac", Channels: 2, ChannelLayout: "stereo"}},
		Video: []probe.VideoStream{{Codec: "mjpeg", IsAttachedPic: true}}}
	p, plan = compose(t, MediaMusic, "opus", song, "a.opus")
	if len(plan.Problems) > 0 || !p.CoverTag || !slices.Contains(p.Tracks, "-v") || !strings.Contains(strings.Join(p.Args, " "), "-c:a:0 libopus -b:a:0 160k") {
		t.Fatalf("music defaults: %+v %+v", p, plan)
	}
	if p, _ := compose(t, MediaMusic, "aac 128k", song, "a.m4a"); p.CoverTag || slices.Contains(p.Tracks, "-v") || !slices.Contains(p.Args, "128k") {
		t.Fatalf("m4a keeps the cover stream: %+v", p)
	}
	if p, _ := compose(t, MediaMusic, "opus -exif", song, "a.opus"); p.CoverTag || !slices.Contains(p.Args, "-map_metadata") {
		t.Fatalf("-exif removes tags and cover: %+v", p)
	}
	for tokens, want := range map[string]string{"opus q5": "no quality scale", "160k": "lossless", "mp3 q12": "0..9"} {
		output := "a.flac"
		if strings.HasPrefix(tokens, "opus") {
			output = "a.opus"
		} else if strings.HasPrefix(tokens, "mp3") {
			output = "a.mp3"
		}
		if _, plan := compose(t, MediaMusic, tokens, song, output); !strings.Contains(strings.Join(plan.Problems, ""), want) {
			t.Errorf("%s: %v", tokens, plan.Problems)
		}
	}
}
