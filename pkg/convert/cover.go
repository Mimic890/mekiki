package convert

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Mimic890/mekiki/pkg/probe"
)

// Cover art reaches ffmpeg as a picture stream, but Matroska stores it as an
// attachment. Mapped like a stream it would become a one-frame video track, or be
// encoded by the video encoder; so a Matroska output takes each cover out of the
// mapping and attaches its original bytes again.

// attachment is one cover picture that a Matroska output attaches again.
type attachment struct {
	index int    // input stream index, taken out of the mapping
	name  string // file name in the output; -metadata finds the new stream by it
	mime  string
	path  string // the extracted picture; Run sets it, a plan shows the name
}

// pictureTypes maps a picture codec to its MIME type and file extension.
var pictureTypes = map[string][2]string{
	"mjpeg": {"image/jpeg", ".jpg"},
	"png":   {"image/png", ".png"},
	"webp":  {"image/webp", ".webp"},
	"bmp":   {"image/bmp", ".bmp"},
	"gif":   {"image/gif", ".gif"},
}

// attachments lists the cover pictures of info that the track selectors keep,
// when output is a Matroska file, and nil otherwise.
func attachments(info *probe.MediaInfo, tracks []string, output string) []attachment {
	if ext := strings.ToLower(filepath.Ext(output)); ext != ".mkv" && ext != ".mka" {
		return nil
	}
	var list []attachment
	used := map[string]bool{}
	for i, v := range info.Video {
		if !v.IsAttachedPic || !kept(tracks, v.Index, "v", i) {
			continue
		}
		mime, ext := tag(v.Tags, "mimetype"), pictureTypes[v.Codec][1]
		if mime == "" {
			mime = pictureTypes[v.Codec][0]
		}
		if mime == "" {
			mime, ext = "application/octet-stream", ".bin"
		}
		name := filepath.Base(tag(v.Tags, "filename"))
		if name == "." || strings.ContainsAny(name, `:/\`) { // ":" would end the stream specifier
			name = "cover" + ext
		}
		if used[name] {
			name = "cover-" + strconv.Itoa(v.Index) + ext
		}
		used[name] = true
		list = append(list, attachment{index: v.Index, name: name, mime: mime, path: name})
	}
	return list
}

// kept reports whether track selectors keep a stream, given its index and its
// position among the streams of its type. No selectors keep everything.
func kept(tracks []string, index int, typ string, pos int) bool {
	keep := len(tracks) == 0
	for _, sel := range tracks {
		s, removes := strings.CutPrefix(sel, "-")
		t, n, hasN := strings.Cut(s, ":")
		if s == "all" || s == strconv.Itoa(index) || t == typ && (!hasN || n == "all" || n == strconv.Itoa(pos)) {
			keep = !removes
		}
	}
	return keep
}

// tag looks up a stream tag; Matroska writes them in upper case, ffprobe reads
// attachments in lower case.
func tag(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// extractPicture returns the bytes of picture stream index as stored in the file.
func extractPicture(ctx context.Context, ffmpeg, input string, index int) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-i", localPath(input),
		"-map", "0:"+strconv.Itoa(index), "-c", "copy", "-f", "image2pipe", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cover art of %q: %s", filepath.Base(input), ffmpegError(stderr.String(), err))
	}
	return data, nil
}
