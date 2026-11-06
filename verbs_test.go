package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mimic890/mekiki/internal/testmedia"
	"github.com/Mimic890/mekiki/pkg/probe"
)

func TestVerbUsageErrors(t *testing.T) {
	isolateConfig(t)
	chdir(t, t.TempDir())
	os.WriteFile("in.mkv", nil, 0o600)
	ctx := context.Background()
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"video", "in.mkv"}, "name at least one change"},
		{[]string{"video", "h265"}, "need at least one input"},
		{[]string{"video", "in.mkv", "h256"}, "did you mean"},
		{[]string{"video", "in.mkv", "h265", "av1"}, "conflict"},
		{[]string{"video", "gone.mkv", "h265"}, "no such file"},
		{[]string{"photo", "in.mkv", "-bogus"}, "removal tokens"},
		{[]string{"shrink", "in.mkv", "h265"}, "takes only existing files"},
		{[]string{"video", "in.mkv", "h265", "-j", "0"}, "--jobs"},
		{[]string{"video", "h265", "--save", "q21"}, "already a token"},
		{[]string{"video", "h265", "--save", "remux"}, "built-in"},
	} {
		code, _, errOut := runApp(ctx, test.args...)
		if code != exitUsage || !strings.Contains(errOut, test.want) {
			t.Errorf("%v: code %d, stderr %q, want %q", test.args, code, errOut, test.want)
		}
	}
	if code, out, _ := runApp(ctx, "music", "-h"); code != exitOK || !strings.Contains(out, "a:rus=opus") {
		t.Errorf("help: %d %q", code, out)
	}
}

func TestVideoVerb(t *testing.T) {
	isolateConfig(t)
	src := testmedia.Generate(t, "film.mkv", "-c:v", "libx264", "-c:a", "flac", "-metadata", "title=Film", "-metadata:s:a:0", "language=eng")
	chdir(t, filepath.Dir(src))
	ctx := context.Background()

	// A dash token is not a flag, in any position.
	code, out, errOut := runApp(ctx, "video", "--explain", "-a:eng", "film.mkv", "h265", "q28")
	if code != exitOK || !strings.Contains(out, "h264→h265 q28") || !strings.Contains(out, "eng flac mono → remove") || !strings.Contains(out, "-map -0:a:0") {
		t.Fatalf("explain: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat("converted"); !os.IsNotExist(err) {
		t.Fatal("--explain must not write")
	}

	code, _, errOut = runApp(ctx, "video", "film.mkv", "a:aac", "--save", "mine")
	if code != exitOK || !strings.Contains(errOut, "aac mono") {
		t.Fatalf("run: %d %q", code, errOut)
	}
	info, err := probe.ScanFile("converted/film.mkv")
	if err != nil || info.Video[0].Codec != "h264" || info.Audio[0].Codec != "aac" || info.Audio[0].Language != "eng" || info.Tags["title"] != "Film" {
		t.Fatalf("output: %+v, %v", info, err)
	}
	if code, _, errOut := runApp(ctx, "video", "film.mkv", "a:aac"); code != exitError || !strings.Contains(errOut, "--overwrite") {
		t.Fatalf("existing output: %d %q", code, errOut)
	}

	// The saved preset and a manual token mix, and a JSON dry run has the plan.
	code, out, errOut = runApp(ctx, "video", "film.mkv", "mine", "a:64k", "--dry-run", "--json")
	var doc struct {
		Jobs []struct {
			Command []string
			Plan    struct {
				Lines []struct{ Kind, Text string }
			}
		}
	}
	if code != exitOK || json.Unmarshal([]byte(out), &doc) != nil || !strings.Contains(strings.Join(doc.Jobs[0].Command, " "), "-c:a:0 aac -b:a:0 64k") || doc.Jobs[0].Plan.Lines[0].Kind != "video" {
		t.Fatalf("preset with manual token: %d %q %q", code, out, errOut)
	}

	// A decision stops everything, with the replacement in the message.
	code, _, errOut = runApp(ctx, "video", "film.mkv", "-o", "x.mp4", "h264")
	if code != exitUsage || !strings.Contains(errOut, "flac does not fit in mp4: replace it with a:aac") {
		t.Fatalf("flac in mp4: %d %q", code, errOut)
	}
	if _, err := os.Stat("x.mp4"); !os.IsNotExist(err) {
		t.Fatal("nothing may be written")
	}
	code, _, errOut = runApp(ctx, "video", "film.mkv", "-o", "x.mp4", "a:aac", "--json", "q99")
	if code != exitUsage || !strings.Contains(errOut, `"code":"usage"`) || !strings.Contains(errOut, "q99") {
		t.Fatalf("json problem: %d %q", code, errOut)
	}
}

func TestVideoVerbKeepsHDR(t *testing.T) {
	isolateConfig(t)
	const master = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"
	src := testmedia.Generate(t, "hdr.mkv", "-c:a", "aac", "-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-x265-params",
		"log-level=error:hdr10=1:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:master-display="+master+":max-cll=1000,400",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc")
	chdir(t, filepath.Dir(src))

	code, out, errOut := runApp(context.Background(), "video", "hdr.mkv", "h265", "q30", "--explain")
	if code != exitOK || !strings.Contains(out, "10bit HDR10: as is") || !strings.Contains(out, "-pix_fmt:v:0 yuv420p10le") ||
		!strings.Contains(out, "master-display="+master) || !strings.Contains(out, "max-cll=1000,400") || !strings.Contains(out, ":pools=") {
		t.Fatalf("explain: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := runApp(context.Background(), "video", "hdr.mkv", "h265", "q30"); code != exitOK {
		t.Fatalf("run: %d %q", code, errOut)
	}
	info, err := probe.ScanFile("converted/hdr.mkv")
	if err != nil || info.Video[0].PixFmt != "yuv420p10le" || !info.Video[0].HDR.IsHDR10 {
		t.Fatalf("output: %+v, %v", info, err)
	}
	h := info.Video[0].HDR
	if err := probe.ReadFrameHDR(context.Background(), "converted/hdr.mkv", 0, &h); err != nil || h.MasterDisplay != master || h.MaxCLL != 1000 {
		t.Fatalf("metadata: %+v, %v", h, err)
	}
}

func TestMusicVerbKeepsCover(t *testing.T) {
	isolateConfig(t)
	cover := testmedia.Generate(t, "cover.jpg", "-q:v", "1")
	src := testmedia.Generate(t, "song.flac", "-i", cover, "-map", "0", "-map", "1", "-c:v", "copy", "-disposition:v", "attached_pic", "-metadata", "title=Song", "-metadata", "artist=Ann")
	chdir(t, filepath.Dir(src))
	ctx := context.Background()

	if code, _, errOut := runApp(ctx, "music", "song.flac", "opus", "160k"); code != exitOK {
		t.Fatalf("opus: %d %q", code, errOut)
	}
	info, err := probe.ScanFile("converted/song.opus")
	if err != nil || info.Audio[0].Codec != "opus" || len(info.Video) != 1 || !info.Video[0].IsAttachedPic {
		t.Fatalf("opus output: %+v, %v", info, err)
	}
	if info.Audio[0].Tags["title"] != "Song" && info.Tags["title"] != "Song" {
		t.Fatalf("title lost: %+v", info)
	}
	want, _ := os.ReadFile(cover)
	got, err := exec.Command("ffmpeg", "-v", "error", "-i", "converted/song.opus", "-map", "0:v", "-c", "copy", "-f", "image2pipe", "-").Output()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("cover changed: %d bytes, want %d; %v", len(got), len(want), err)
	}

	if code, _, errOut := runApp(ctx, "music", "song.flac", "-exif", "-o", "clean.flac"); code != exitOK {
		t.Fatalf("-exif: %d %q", code, errOut)
	}
	info, err = probe.ScanFile("clean.flac")
	if err != nil || len(info.Video) != 0 || info.Tags["title"] != "" {
		t.Fatalf("clean output: %+v, %v", info, err)
	}
}

func TestPhotoVerb(t *testing.T) {
	isolateConfig(t)
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed")
	}
	src := testmedia.Generate(t, "a.jpg")
	chdir(t, filepath.Dir(src))
	if out, err := exec.Command("exiftool", "-q", "-overwrite_original", "-Orientation#=6", "-Artist=Ann", "-GPSLatitude=55.7", "-GPSLatitudeRef=N", "a.jpg").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	tag := func(path, name string) string {
		out, _ := exec.Command("exiftool", "-s3", "-"+name, path).Output()
		return strings.TrimSpace(string(out))
	}
	ctx := context.Background()

	if code, _, errOut := runApp(ctx, "photo", "a.jpg", "webp", "50%", "q85"); code != exitOK {
		t.Fatalf("keep: %d %q", code, errOut)
	}
	if out := "converted/a.webp"; tag(out, "Artist") != "Ann" || tag(out, "GPSLatitude") == "" || tag(out, "Orientation") != "Rotate 90 CW" || tag(out, "ImageSize") != "32x24" {
		t.Fatalf("tags: %s %s %s %s", tag(out, "Artist"), tag(out, "GPSLatitude"), tag(out, "Orientation"), tag(out, "ImageSize"))
	}
	if code, _, errOut := runApp(ctx, "photo", "a.jpg", "-exif", "-o", "clean.jpg"); code != exitOK {
		t.Fatalf("strip: %d %q", code, errOut)
	}
	if tag("clean.jpg", "Artist") != "" || tag("clean.jpg", "GPSLatitude") != "" || tag("clean.jpg", "Orientation") != "Rotate 90 CW" {
		t.Fatalf("clean: %s %s %s", tag("clean.jpg", "Artist"), tag("clean.jpg", "GPSLatitude"), tag("clean.jpg", "Orientation"))
	}
}

func TestShrink(t *testing.T) {
	isolateConfig(t)
	src := testmedia.Generate(t, "big.mp4", "-s", "640x480", "-c:v", "libx264", "-crf", "0", "-c:a", "aac")
	dir := filepath.Dir(src)
	chdir(t, dir)
	os.WriteFile("broken.mkv", []byte("junk"), 0o600)
	ctx := context.Background()

	code, _, errOut := runApp(ctx, "shrink", ".")
	if code != exitOK || !strings.Contains(errOut, "skipping") {
		t.Fatalf("shrink: %d %q", code, errOut)
	}
	info, err := probe.ScanFile("converted/big.mp4")
	if err != nil || info.Video[0].Codec != "hevc" || info.Audio[0].Codec != "aac" {
		t.Fatalf("output: %+v, %v", info, err)
	}

	// The result of squeezing an already tiny file is thrown away, without failing.
	tiny := testmedia.Generate(t, "tiny.mkv", "-c:v", "libx265", "-crf", "51", "-x265-params", "log-level=error", "-an")
	code, stdout, errOut := runApp(ctx, "shrink", tiny, "-o", filepath.Join(dir, "out.mkv"), "--json")
	if code != exitOK || !strings.Contains(stdout, `"type":"skipped"`) {
		t.Fatalf("tiny: %d %q %q", code, stdout, errOut)
	}
	if _, err := os.Stat("out.mkv"); !os.IsNotExist(err) {
		t.Fatal("a file that is not smaller must not be written")
	}
	assertNoPartFiles(t, dir)
}

func assertNoPartFiles(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
