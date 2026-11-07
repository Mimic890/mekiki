package convert

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// BuildArgs returns the ffmpeg arguments (without the program name) that
// convert input to output with preset p. Both are local file paths, never
// protocol URLs. threads > 0 sets the CPU thread budget.
func BuildArgs(p Preset, input, output string, threads int) ([]string, error) {
	args := []string{"-hide_banner", "-nostdin", "-y", "-v", "error", "-progress", "pipe:1", "-nostats"}
	args = append(args, p.InputArgs...)
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads)) // decoder threads
	}
	args = append(args, "-i", localPath(input))
	if p.CoverTag {
		args = append(args, "-f", "ffmetadata", "-i", "pipe:0") // fed by Run
	}

	if len(p.Tracks) == 0 {
		args = append(args, "-map", "0")
	}
	for _, sel := range p.Tracks {
		m, err := mapArg(sel)
		if err != nil {
			return nil, err
		}
		args = append(args, "-map", m)
	}
	for _, a := range p.attach {
		args = append(args, "-map", "-0:"+strconv.Itoa(a.index)) // attached again below, never encoded
	}
	for _, i := range p.covers {
		args = append(args, "-map", "-0:"+strconv.Itoa(i))
	}
	for _, i := range p.covers {
		args = append(args, "-map", "0:"+strconv.Itoa(i)) // last, so -c:v:N below finds them
	}

	// Copy everything by default (attachments, cover art, data), then pick encoders per type.
	args = append(args, "-c", "copy")
	videoSpec := "V"
	if p.VideoStream != "" {
		videoSpec = p.VideoStream
	}
	for _, c := range []struct {
		spec  string
		codec Codec
	}{{videoSpec, p.Video}, {"a", p.Audio}, {"s", p.Subtitle}} {
		ca, err := codecArgs(c.spec, c.codec, threads)
		if err != nil {
			return nil, fmt.Errorf("preset %s: %w", p.Name, err)
		}
		args = append(args, ca...)
	}
	// "V" leaves out cover art only as ffmpeg sees it on the output, which misses a
	// Matroska cover going to MP4; the last matching -c wins.
	for n := range p.covers {
		args = append(args, "-c:v:"+strconv.Itoa(p.videos+n), "copy")
	}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads)) // encoder threads
	}
	if p.CoverTag {
		args = append(args, "-map_metadata", "0", "-map_metadata", "1")
	}
	args = append(args, p.Args...)
	for _, a := range p.attach {
		args = append(args, "-attach", localPath(a.path), "-metadata:s:t:m:filename:"+a.name, "mimetype="+a.mime)
	}
	return append(args, localPath(output)), nil
}

// localPath keeps ffmpeg from reading a relative file name such as
// "pipe:1.mkv" or "concat:a.mkv|b.mkv" as a protocol URL.
func localPath(path string) string {
	if strings.Contains(path, ":") && !filepath.IsAbs(path) {
		return "file:" + path
	}
	return path
}

func codecArgs(spec string, c Codec, threads int) ([]string, error) {
	if c.Codec == "" {
		return nil, nil
	}
	args := []string{"-c:" + spec, c.Codec}
	if c.Codec == "copy" {
		return args, nil
	}
	if c.CRF != nil {
		q, err := qualityArgs(c.Codec, spec, *c.CRF, c.Bitrate == "")
		if err != nil {
			return nil, err
		}
		args = append(args, q...)
	}
	if c.Bitrate != "" {
		args = append(args, "-b:"+spec, c.Bitrate)
	}
	if c.Preset != "" {
		args = append(args, "-preset:"+spec, c.Preset)
	}
	if c.PixFmt != "" {
		args = append(args, "-pix_fmt:"+spec, c.PixFmt)
	}
	// x265 and SVT-AV1 ignore -threads and size their own pools.
	if threads > 0 {
		t := strconv.Itoa(threads)
		i := slices.Index(c.Args, "-x265-params")
		switch {
		case c.Codec == "libx265" && i < 0:
			args = append(args, "-x265-params", "pools="+t)
		case c.Codec == "libx265" && !strings.Contains(c.Args[i+1], "pools="):
			c.Args = slices.Clone(c.Args)
			c.Args[i+1] += ":pools=" + t
		case c.Codec == "libsvtav1" && !contains(c.Args, "-svtav1-params"):
			args = append(args, "-svtav1-params", "lp="+t)
		}
	}
	return append(args, c.Args...), nil
}

// qualityArgs maps the preset's crf value to each encoder's constant-quality option.
func qualityArgs(encoder, spec string, q int, noBitrate bool) ([]string, error) {
	n := strconv.Itoa(q)
	switch {
	case contains([]string{"libx264", "libx265", "libsvtav1"}, encoder):
		return []string{"-crf:" + spec, n}, nil
	case encoder == "libaom-av1" || encoder == "libvpx-vp9":
		args := []string{"-crf:" + spec, n}
		if noBitrate {
			args = append(args, "-b:"+spec, "0") // constant quality instead of constrained bitrate
		}
		return args, nil
	case strings.HasSuffix(encoder, "_nvenc"):
		args := []string{"-rc:" + spec, "vbr", "-cq:" + spec, n}
		if noBitrate {
			args = append(args, "-b:"+spec, "0")
		}
		return args, nil
	case strings.HasSuffix(encoder, "_qsv"):
		return []string{"-global_quality:" + spec, n}, nil
	case strings.HasSuffix(encoder, "_vaapi"):
		return []string{"-rc_mode:" + spec, "CQP", "-qp:" + spec, n}, nil
	case strings.HasSuffix(encoder, "_amf"):
		return []string{"-rc:" + spec, "cqp", "-qp_i:" + spec, n, "-qp_p:" + spec, n}, nil
	case encoder == "libwebp":
		return []string{"-quality:" + spec, n}, nil
	case encoder == "mjpeg" || encoder == "libmp3lame":
		return []string{"-q:" + spec, n}, nil
	}
	return nil, fmt.Errorf("crf is not supported for encoder %s; set a bitrate or raw args instead", encoder)
}

// mapArg turns a track selector into an ffmpeg -map value.
// Selectors: "all", a stream index ("3"), a type ("v", "a", "s", "t") optionally
// followed by ":N" (Nth stream of that type) or ":all". Whole-type selectors
// are optional, so files without such tracks still convert. A leading "-" on a
// type selector removes those tracks from what earlier selectors picked.
func mapArg(sel string) (string, error) {
	if sel == "all" {
		return "0", nil
	}
	if rest, ok := strings.CutPrefix(sel, "-"); ok {
		m, err := mapArg(rest)
		if err != nil || !strings.Contains(m, ":") || m[2] < 'a' {
			return "", fmt.Errorf("track selector %q: use -TYPE or -TYPE:N", sel)
		}
		return "-" + strings.TrimSuffix(m, "?"), nil
	}
	if n, err := strconv.Atoi(sel); err == nil && n >= 0 {
		return "0:" + sel, nil
	}
	typ, idx, hasIdx := strings.Cut(sel, ":")
	if !contains([]string{"v", "a", "s", "t"}, typ) {
		return "", fmt.Errorf("track selector %q: use all, N, v, a, s, t, TYPE:N or TYPE:all", sel)
	}
	if !hasIdx || idx == "all" {
		return "0:" + typ + "?", nil
	}
	if n, err := strconv.Atoi(idx); err != nil || n < 0 {
		return "", fmt.Errorf("track selector %q: %q is not a track number", sel, idx)
	}
	return "0:" + typ + ":" + idx, nil
}
