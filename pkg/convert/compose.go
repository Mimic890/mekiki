package convert

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Mimic890/mekiki/pkg/meta"
	"github.com/Mimic890/mekiki/pkg/probe"
	"github.com/Mimic890/mekiki/pkg/tools"
)

// FilePlan is what Compose decided for one file. A plan with Problems must not
// run: each one needs a decision from the user.
type FilePlan struct {
	Lines    []PlanLine `json:"lines"`
	Warnings []string   `json:"warnings,omitempty"`
	Problems []string   `json:"-"`
}

// PlanLine is one row of the plan, such as kind "audio" and "eng dts → remove".
type PlanLine struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// qualityScale is an encoder's q range and the value the best token stands for.
type qualityScale struct{ best, lo, hi int }

var qualityScales = map[string]qualityScale{
	"libx264": {17, 0, 51}, "libx265": {18, 0, 51}, "libsvtav1": {24, 0, 63}, "libaom-av1": {20, 0, 63},
	"hevc_nvenc": {20, 0, 51}, "av1_nvenc": {24, 0, 51}, "hevc_qsv": {20, 1, 51}, "av1_qsv": {24, 1, 51},
	"hevc_amf": {20, 0, 51}, "av1_amf": {90, 0, 255}, "hevc_vaapi": {20, 0, 51}, "av1_vaapi": {90, 0, 255},
	"libwebp": {95, 0, 100}, "mjpeg": {2, 1, 31}, "libmp3lame": {0, 0, 9},
}

// bestBitrates stands in for the best token on audio encoders without a quality scale.
var bestBitrates = map[string]string{"libopus": "256k", "aac": "320k"}

// A containerRule lists what a container takes; nil means no check. hint is the replacement to offer.
type containerRule struct {
	audioOnly, audioNever, subs, video []string
	hint                               string
}

var (
	mp4Rule        = containerRule{audioNever: []string{"flac", "vorbis", "truehd", "dts"}, subs: []string{"mov_text"}, hint: "a:aac"}
	containerRules = map[string]containerRule{
		".mp4": mp4Rule, ".m4v": mp4Rule, ".mov": mp4Rule,
		".m4a":  {audioOnly: []string{"aac", "alac"}, hint: "a:aac"},
		".webm": {audioOnly: []string{"opus", "vorbis"}, subs: []string{"webvtt"}, video: []string{"vp8", "vp9", "av1"}, hint: "a:opus"},
		".opus": {audioOnly: []string{"opus"}, hint: "a:opus"},
		".ogg":  {audioOnly: []string{"opus", "vorbis", "flac"}, hint: "a:opus"},
		".oga":  {audioOnly: []string{"opus", "vorbis", "flac"}, hint: "a:opus"},
		".mp3":  {audioOnly: []string{"mp3"}, hint: "a:mp3"},
		".flac": {audioOnly: []string{"flac"}, hint: "a:flac"},
	}
	coverStreamExts = []string{".flac", ".mp3", ".m4a", ".mp4", ".mka", ".mkv"}
	coverTagExts    = []string{".opus", ".ogg", ".oga"}
	photoSources    = map[string]string{"mjpeg": "jpeg", "png": "png", "webp": "webp", "av1": "avif"}
)

// Kind returns the media type of a probed file, "" when it has nothing mekiki can convert.
func Kind(info *probe.MediaInfo) string {
	if meta.IsImage(info.FilePath) {
		return MediaPhoto
	}
	if _, v := mainVideo(info); v != nil {
		return MediaVideo
	}
	if len(info.Audio) > 0 {
		return MediaMusic
	}
	return ""
}

// mainVideo returns the first video stream that is not cover art and its position among video streams.
func mainVideo(info *probe.MediaInfo) (int, *probe.VideoStream) {
	for i := range info.Video {
		if !info.Video[i].IsAttachedPic {
			return i, &info.Video[i]
		}
	}
	return 0, nil
}

// ContainerFor returns the output extension the tokens call for, "" to keep the input's.
func (l *Library) ContainerFor(media string, spec Spec) string {
	name := ""
	switch media {
	case MediaPhoto:
		name = photoCodecs[spec["v"]]
	case MediaMusic:
		name = tokenValue(spec["a"])
	}
	if name == "" || name == "copy" {
		return ""
	}
	p, err := l.Get(name)
	if err != nil {
		return ""
	}
	return p.Container
}

// Compose fills everything the tokens leave open so that nothing is lost, and
// returns the preset for one file and the plan that describes it.
func (l *Library) Compose(ctx context.Context, media string, spec Spec, info *probe.MediaInfo, output string) (Preset, *FilePlan) {
	c := &composer{l: l, ctx: ctx, media: media, spec: spec, info: info, ext: strings.ToLower(filepath.Ext(output)), plan: &FilePlan{}}
	c.p = Preset{Name: verbName(media), Media: media, Load: LoadIO}
	kind := Kind(info)
	if kind != media {
		verb := verbName(kind)
		if verb == "" {
			c.problem("has no streams mekiki can convert")
		} else {
			c.problem("is a %s file; use mekiki %s", verb, verb)
		}
		return c.p, c.plan
	}
	switch media {
	case MediaVideo:
		c.video()
		c.audio()
		c.subs()
		if spec["exif"] != "" {
			c.p.Args = append(c.p.Args, "-map_metadata:g", "-1")
			c.line("meta", "tags → remove")
		}
	case MediaPhoto:
		c.photo()
	case MediaMusic:
		c.audio()
		c.music()
	}
	if _, err := BuildArgs(c.p, "in", "out", 0); err != nil {
		c.problem("%v", err)
	}
	c.warnMatroskaCover()
	c.checkEncoders()
	if err := c.p.Normalize(); err != nil {
		c.problem("%v", err)
	}
	return c.p, c.plan
}

type composer struct {
	l        *Library
	ctx      context.Context
	media    string
	spec     Spec
	info     *probe.MediaInfo
	ext      string
	p        Preset
	plan     *FilePlan
	encoders []string
}

func (c *composer) line(kind, format string, a ...any) {
	c.plan.Lines = append(c.plan.Lines, PlanLine{kind, fmt.Sprintf(format, a...)})
}
func (c *composer) warn(format string, a ...any) {
	c.plan.Warnings = append(c.plan.Warnings, fmt.Sprintf(format, a...))
}
func (c *composer) problem(format string, a ...any) {
	if msg := fmt.Sprintf(format, a...); !slices.Contains(c.plan.Problems, msg) { // two tracks, one decision
		c.plan.Problems = append(c.plan.Problems, msg)
	}
}

// shortcut: the cover keeps its bytes but changes shape; to keep it an attachment, Run would
// have to extract it to a temp file and add it back with -attach.
func (c *composer) warnMatroskaCover() {
	if c.ext != ".mkv" && c.ext != ".mka" || slices.Contains(c.p.Tracks, "-v") {
		return
	}
	for _, v := range c.info.Video {
		if v.IsAttachedPic {
			c.warn("ffmpeg writes %s cover art to %s as a one-frame video track, not as an attachment", v.Codec, strings.TrimPrefix(c.ext, "."))
			return
		}
	}
}

func (c *composer) checkEncoders() {
	ffmpeg, err := tools.Path("ffmpeg")
	if err != nil {
		c.problem("%v", err)
		return
	}
	available, err := listEncoders(ffmpeg)
	if err != nil {
		c.problem("%v", err)
		return
	}
	for _, enc := range c.encoders {
		if !available[enc] {
			c.problem("encoder %s is not included in this ffmpeg build", enc)
		}
	}
}

// ── Video ────────────────────────────────────────────────────────────────────

func (c *composer) video() {
	k, v := mainVideo(c.info)
	w, h := v.Width, v.Height
	if v.Rotation%180 != 0 { // ffmpeg rotates before filtering
		w, h = h, w
	}
	newW, newH := fitSize(c.spec["size"], w, h)
	fps := 0.0
	if tok := c.spec["fps"]; tok != "" {
		fps, _ = strconv.ParseFloat(strings.TrimSuffix(tok, "fps"), 64)
		if math.Abs(fps-v.FPS) < 0.01 {
			fps = 0
		}
	}

	var changes, kept []string
	size := fmt.Sprintf("%dx%d", w, h)
	if newW != w || newH != h {
		size = fmt.Sprintf("%s→%dx%d", size, newW, newH)
		changes = append(changes, size)
	} else {
		kept = append(kept, size)
	}
	if fps != 0 {
		changes = append(changes, fmt.Sprintf("%g→%gfps", v.FPS, fps))
	} else if v.FPS > 0 {
		kept = append(kept, fmt.Sprintf("%g", v.FPS))
	}

	encode := c.spec["v"] != "" || c.spec["q"] != "" || c.spec["gpu"] != "" || len(changes) > 0
	if !encode {
		c.keepDepthAndHDR(v, v.BitDepth, &kept)
		c.line("video", "%s %s: as is", v.Codec, strings.Join(kept, " "))
		c.checkVideoFit(v.Codec)
		return
	}

	name := c.spec["v"]
	if name == "" {
		if videoCodecs[v.Codec] == "" {
			c.problem("%s has no default encoder here; name a codec: h265, h264 or av1", v.Codec)
			return
		}
		name = map[string]string{"hevc": "h265"}[v.Codec]
		if name == "" {
			name = v.Codec
		}
	}
	family := videoCodecs[name]
	base, err := c.encoderPreset(family)
	if err != nil {
		c.problem("%v", err)
		return
	}
	vc := base.Video
	quality := c.quality(&vc)
	label := name
	if c.spec["gpu"] != "" {
		label += " (" + vc.Codec + ")"
	}
	changes = append([]string{v.Codec + "→" + label + " " + quality}, changes...)

	var filters []string
	if newW != w || newH != h {
		filters = append(filters, fmt.Sprintf("scale=%d:%d", newW, newH))
	}
	if fps != 0 {
		filters = append(filters, fmt.Sprintf("fps=%g", fps))
	}
	args := slices.Clone(base.Args)
	if i := slices.Index(args, "-vf"); i >= 0 { // hardware upload stays the last step of the chain
		upload := args[i+1]
		if v.BitDepth > 8 {
			upload = strings.Replace(upload, "nv12", "p010", 1)
		}
		filters = append(filters, upload)
		args = slices.Delete(args, i, i+2)
	}

	depth := c.pixelFormat(&vc, v)
	if depth != v.BitDepth {
		changes = append(changes, fmt.Sprintf("%dbit→%dbit", v.BitDepth, depth))
		depth = 0
	}
	c.keepDepthAndHDR(v, depth, &kept)
	c.encodeHDR(&vc, v, k)

	c.p.Video = vc
	c.p.VideoStream = "v:" + strconv.Itoa(k)
	c.p.InputArgs = base.InputArgs
	c.p.Load = base.Load
	c.p.Args = args
	if len(filters) > 0 {
		c.p.Args = append(c.p.Args, "-filter:"+c.p.VideoStream, strings.Join(filters, ","))
	}
	c.encoders = append(c.encoders, vc.Codec)

	text := strings.Join(changes, " ")
	if len(kept) > 0 {
		text += "   " + strings.Join(kept, " ") + ": as is"
	}
	c.line("video", "%s", text)
	c.checkVideoFit(family)
}

func (c *composer) keepDepthAndHDR(v *probe.VideoStream, depth int, kept *[]string) {
	if depth > 8 {
		*kept = append(*kept, fmt.Sprintf("%dbit", depth))
	}
	switch {
	case v.HDR.IsDolbyVision:
		*kept = append(*kept, "DolbyVision")
	case v.HDR.IsHDR10Plus:
		*kept = append(*kept, "HDR10+")
	case v.HDR.IsHDR10:
		*kept = append(*kept, "HDR10")
	case v.HDR.IsHLG:
		*kept = append(*kept, "HLG")
	}
}

// encoderPreset picks the built-in settings of a codec family, on the GPU with the gpu token.
func (c *composer) encoderPreset(family string) (Preset, error) {
	if c.spec["gpu"] == "" {
		return c.l.Get(family)
	}
	l := c.l
	l.gpuMu.Lock() // concurrent Compose calls share one test encode per family
	defer l.gpuMu.Unlock()
	if _, tested := l.gpu[family]; !tested {
		var found *Preset
		for _, name := range l.Names() {
			p, err := l.Get(name)
			if err == nil && p.Load == LoadGPU && strings.HasPrefix(name, family+"-") && Verify(c.ctx, p) == nil {
				found = &p
				break
			}
		}
		if err := c.ctx.Err(); err != nil {
			return Preset{}, err // a canceled test encode says nothing about the GPU
		}
		if l.gpu == nil {
			l.gpu = map[string]*Preset{}
		}
		l.gpu[family] = found
	}
	if p := l.gpu[family]; p != nil {
		return *p, nil
	}
	return Preset{}, fmt.Errorf("gpu: no GPU preset for %s works on this machine; see mekiki doctor", family)
}

// quality applies the q slot to a codec and returns its label for the plan.
func (c *composer) quality(vc *Codec) string {
	tok := c.spec["q"]
	scale, scaled := qualityScales[vc.Codec]
	switch {
	case tok == "" || tok == "good":
		return "good"
	case !scaled:
		c.problem("%s has no quality scale, so %s means nothing", vc.Codec, tok)
		return tok
	case tok == "best":
		vc.CRF = &scale.best
		return tok
	}
	n, _ := strconv.Atoi(tok[1:])
	if n < scale.lo || n > scale.hi {
		c.problem("%s is out of range for %s: the scale is %d..%d, best is %d", tok, vc.Codec, scale.lo, scale.hi, scale.best)
		return tok
	}
	vc.CRF = &n
	return tok
}

// pixelFormat keeps the source's high bit depth and returns the depth the output will have.
func (c *composer) pixelFormat(vc *Codec, v *probe.VideoStream) int {
	if v.BitDepth <= 8 {
		return v.BitDepth
	}
	pix, depth := v.PixFmt, v.BitDepth
	switch {
	case strings.HasSuffix(vc.Codec, "_vaapi"): // the format comes from the upload filter
		return min(depth, 10)
	case strings.Contains(vc.Codec, "_"):
		pix, depth = "p010le", min(depth, 10)
	case vc.Codec == "libsvtav1":
		pix, depth = "yuv420p10le", min(depth, 10)
	case vc.Codec == "libx264" && depth > 10:
		pix, depth = "yuv420p10le", 10
	}
	if pix != v.PixFmt && !strings.HasPrefix(v.PixFmt, "yuv420p") && !strings.HasPrefix(v.PixFmt, "p01") {
		c.warn("pixel format changes from %s to %s: %s supports nothing closer", v.PixFmt, pix, vc.Codec)
	}
	vc.PixFmt = pix
	return depth
}

// encodeHDR carries HDR over into the new encode, or says what cannot be carried.
func (c *composer) encodeHDR(vc *Codec, v *probe.VideoStream, k int) {
	h := v.HDR
	if h.IsDolbyVision {
		if h.DVCompatibility == "" {
			c.problem("Dolby Vision without a compatible base layer: re-encoding would ruin the colors; do not name a video change")
			return
		}
		c.warn("Dolby Vision cannot be re-encoded: the output keeps only the %s base layer", h.DVCompatibility)
	}
	if !h.IsHDR10 && !h.IsHLG {
		return
	}
	if err := probe.ReadFrameHDR(c.ctx, c.info.FilePath, k, &h); err != nil {
		c.problem("reading HDR metadata: %v", err)
		return
	}
	if h.IsHDR10Plus {
		c.warn("HDR10+ dynamic metadata cannot be re-encoded: static HDR10 stays")
	}
	switch {
	case vc.Codec == "libx265":
		vc.Args = append(slices.Clone(vc.Args), "-x265-params", x265HDR(h))
	case strings.Contains(vc.Codec, "_"):
		c.warn("%s may not write mastering display metadata; color and bit depth stay", vc.Codec)
	}
}

func x265HDR(h probe.HDRInfo) string {
	var params []string
	for _, kv := range [][2]string{{"colorprim", h.ColorPrimaries}, {"transfer", h.ColorTransfer}, {"colormatrix", h.ColorSpace}, {"master-display", h.MasterDisplay}} {
		if kv[1] != "" {
			params = append(params, kv[0]+"="+kv[1])
		}
	}
	if h.IsHDR10 {
		params = append(params, "hdr10=1")
	}
	if h.MaxCLL > 0 {
		params = append(params, fmt.Sprintf("max-cll=%d,%d", h.MaxCLL, h.MaxFALL))
	}
	return strings.Join(params, ":")
}

// fitSize applies a size token; it enlarges only when a percentage asks for it.
func fitSize(tok string, w, h int) (int, int) {
	if tok == "" || w == 0 || h == 0 {
		return w, h
	}
	long, short := float64(max(w, h)), float64(min(w, h))
	r := 1.0
	switch {
	case strings.HasSuffix(tok, "%"):
		n, _ := strconv.Atoi(strings.TrimSuffix(tok, "%"))
		r = float64(n) / 100
	case strings.HasSuffix(tok, "px"):
		n, _ := strconv.Atoi(strings.TrimSuffix(tok, "px"))
		r = min(float64(n)/long, 1)
	default: // 1080p is the 1920x1080 box, turned to fit portrait sources
		n, _ := strconv.Atoi(strings.TrimSuffix(tok, "p"))
		r = min(float64(n)*16/9/long, float64(n)/short, 1)
	}
	if r == 1 {
		return w, h
	}
	even := func(side int) int { return max(int(math.Round(float64(side)*r))&^1, 2) }
	return even(w), even(h)
}

// ── Audio and subtitles ──────────────────────────────────────────────────────

func (c *composer) audio() {
	var items []string
	out := 0
	languages := map[string]bool{}
	dropped := 0
	for i, a := range c.info.Audio {
		lang := a.Language
		if lang == "" {
			lang = "und"
		}
		languages[lang] = true
		desc := lang + " " + a.Codec + " " + channels(a)
		if c.media == MediaMusic && lang == "und" {
			desc = a.Codec + " " + channels(a) // songs rarely tag a language
		}
		named := c.spec["a:"+lang]
		if strings.HasPrefix(named, "-") {
			c.p.Tracks = append(c.p.Tracks, "-a:"+strconv.Itoa(i))
			items = append(items, desc+" → remove")
			dropped++
			continue
		}
		args, text := c.encodeAudio(a, lang, out)
		c.p.Args = append(c.p.Args, args...)
		if text == "" {
			items = append(items, desc+": as is")
		} else {
			items = append(items, desc+" → "+text)
		}
		out++
	}
	if len(items) > 0 {
		c.line("audio", "%s", strings.Join(items, " · "))
	}
	if dropped == len(c.info.Audio) && dropped > 0 {
		c.warn("no audio track is left")
	}
	for _, slot := range sortedKeys(c.spec) {
		lang, ok := strings.CutPrefix(slot, "a:")
		lang = strings.TrimSuffix(lang, ".br")
		if ok && !languages[lang] {
			c.warn("%s has no effect: no %s audio track", c.spec[slot], lang)
		}
	}
	if len(c.p.Tracks) > 0 {
		c.p.Tracks = append([]string{"all"}, c.p.Tracks...)
	}
}

// encodeAudio returns the args that encode audio track out, and the plan text; both empty for a copy.
func (c *composer) encodeAudio(a probe.AudioStream, lang string, out int) ([]string, string) {
	token := tokenValue(c.spec["a:"+lang])
	if token == "" {
		token = tokenValue(c.spec["a"])
	}
	rate := tokenValue(c.spec["a:"+lang+".br"])
	if rate == "" {
		rate = tokenValue(c.spec["a.br"])
	}
	q := ""
	if c.media == MediaMusic {
		q = c.spec["q"]
	}
	rule := c.containerRule()
	target := token
	if target == "" {
		if rate == "" && q == "" {
			c.checkAudioFit(rule, a.Codec)
			return nil, ""
		}
		target = a.Codec
		if audioCodecs[target] == "" {
			c.problem("%s has no default encoder here; name a codec: a:aac %s", a.Codec, strings.TrimSpace(rate))
			return nil, ""
		}
	}
	if target == "copy" || target == a.Codec && rate == "" && (q == "" || q == "good") {
		c.checkAudioFit(rule, a.Codec)
		return nil, "" // already there; encoding again would only lose quality
	}
	enc := audioCodecs[target]
	if enc == "flac" && rate != "" {
		c.problem("flac is lossless, so %s means nothing", rate)
		return nil, ""
	}

	ac := Codec{Codec: enc}
	if c.media == MediaMusic {
		if p, err := c.l.Get(target); err == nil && p.Audio.Codec == enc {
			ac = p.Audio
		}
	}
	label := channels(a)
	switch {
	case rate != "":
		ac.Bitrate, ac.CRF = rate, nil
		label += " " + rate
	case q == "best" && bestBitrates[enc] != "":
		ac.Bitrate = bestBitrates[enc]
		label += " best"
	case q != "" && enc != "flac":
		if bestBitrates[enc] != "" && q != "good" {
			c.problem("%s has no quality scale, so %s means nothing; use a bitrate like 160k", enc, q)
			return nil, ""
		}
		label += " " + c.quality(&ac)
	case ac.Bitrate != "":
		label += " " + ac.Bitrate
	}
	if enc == "libmp3lame" && a.Channels > 2 {
		c.warn("mp3 holds at most stereo: %s is mixed down", channels(a))
		label = strings.Replace(label, channels(a), "stereo", 1)
	}

	spec := "a:" + strconv.Itoa(out)
	args, err := codecArgs(spec, ac, 0)
	if err != nil {
		c.problem("%v", err)
		return nil, ""
	}
	if enc == "libopus" && strings.HasSuffix(a.ChannelLayout, "(side)") {
		// libopus rejects side layouts; the speakers are the same, only named differently.
		args = append(args, "-filter:"+spec, "channelmap=channel_layout="+channels(a))
	}
	if c.p.Load == LoadIO {
		c.p.Load = LoadLight
	}
	c.encoders = append(c.encoders, enc)
	c.checkAudioFit(rule, target)
	return args, target + " " + label
}

func (c *composer) containerRule() containerRule { return containerRules[c.ext] }

func (c *composer) checkAudioFit(rule containerRule, codec string) {
	if rule.audioOnly == nil && rule.audioNever == nil {
		return
	}
	if slices.Contains(rule.audioNever, codec) || rule.audioOnly != nil && !slices.Contains(rule.audioOnly, codec) {
		c.problem("%s does not fit in %s: replace it with %s, or write to .mkv with -o", codec, strings.TrimPrefix(c.ext, "."), rule.hint)
	}
}

func (c *composer) checkVideoFit(codec string) {
	if only := c.containerRule().video; only != nil && !slices.Contains(only, codec) {
		c.problem("video %s does not fit in %s; write to .mkv with -o", codec, strings.TrimPrefix(c.ext, "."))
	}
}

func channels(a probe.AudioStream) string {
	if a.ChannelLayout != "" {
		layout, _, _ := strings.Cut(a.ChannelLayout, "(")
		return layout
	}
	return strconv.Itoa(a.Channels) + "ch"
}

func (c *composer) subs() {
	if len(c.info.Subtitle) == 0 {
		return
	}
	langs := make([]string, len(c.info.Subtitle))
	for i, s := range c.info.Subtitle {
		langs[i] = s.Language
		if langs[i] == "" {
			langs[i] = "und"
		}
	}
	list := strings.Join(langs, ", ")
	if c.spec["s"] != "" {
		if len(c.p.Tracks) == 0 {
			c.p.Tracks = []string{"all"}
		}
		c.p.Tracks = append(c.p.Tracks, "-s")
		c.line("subs", "%s → remove", list)
		return
	}
	c.line("subs", "%s: as is", list)
	if only := c.containerRule().subs; only != nil {
		for _, s := range c.info.Subtitle {
			if !slices.Contains(only, s.Codec) {
				c.problem("%s subtitles do not fit in %s: remove them with -s, or write to .mkv with -o", s.Codec, strings.TrimPrefix(c.ext, "."))
				return
			}
		}
	}
}

// ── Photo and music ──────────────────────────────────────────────────────────

func (c *composer) photo() {
	if len(c.info.Video) == 0 {
		c.problem("has no image stream")
		return
	}
	v := c.info.Video[0]
	if v.Codec == "gif" {
		c.problem("gif can be animated and photo keeps one frame; use mekiki convert")
		return
	}
	target := photoCodecs[c.spec["v"]]
	source := photoSources[v.Codec]
	if target == "" {
		target = source
	}
	newW, newH := fitSize(c.spec["size"], v.Width, v.Height)
	resized := newW != v.Width || newH != v.Height
	encode := target != source || c.spec["q"] != "" || resized
	if target == "" {
		if encode {
			c.problem("%s cannot be written back; name a target: webp, jpeg, avif or png", v.Codec)
			return
		}
		target = "jpeg" // only its single-image settings are used for the copy
	}
	base, err := c.l.Get(target)
	if err != nil {
		c.problem("%v", err)
		return
	}
	if (source != "" || c.spec["v"] != "") && !sameExt(c.ext, base.Container) {
		c.problem("the output %s does not match %s; name the output .%s", c.ext, target, base.Container)
		return
	}

	c.p.Tracks, c.p.Args, c.p.Load = base.Tracks, base.Args, LoadLight
	// Pixels stay as stored and the Orientation tag goes along, so the photo looks the same everywhere.
	c.p.InputArgs = []string{"-noautorotate"}
	c.p.Exif = ExifCopy
	metaText := "EXIF, orientation: as is"
	if c.spec["exif"] != "" {
		c.p.Exif = ExifStrip
		metaText = "EXIF → remove, orientation: as is"
	}

	size := fmt.Sprintf("%dx%d", v.Width, v.Height)
	if !encode {
		c.p.Video = Codec{Codec: "copy"}
		c.line("photo", "%s %s: as is", v.Codec, size)
		c.line("meta", "%s", metaText)
		return
	}
	vc := base.Video
	quality := c.quality(&vc)
	c.p.Video = vc
	c.encoders = append(c.encoders, vc.Codec)
	if resized {
		c.p.Args = append(slices.Clone(c.p.Args), "-filter:V", fmt.Sprintf("scale=%d:%d", newW, newH))
		size = fmt.Sprintf("%s→%dx%d", size, newW, newH)
	}
	if target != "png" && target != "webp" && target != "avif" && hasAlpha(v.PixFmt) {
		c.warn("transparency is lost in %s; webp, avif and png keep it", target)
	}
	if target == "png" {
		quality = "lossless"
	}
	c.line("photo", "%s→%s %s %s", v.Codec, target, quality, size)
	c.line("meta", "%s", metaText)
}

func hasAlpha(pix string) bool {
	return strings.Contains(pix, "a") && (strings.HasPrefix(pix, "yuva") || strings.HasPrefix(pix, "rgba") || strings.HasPrefix(pix, "bgra") || strings.HasPrefix(pix, "ya") || pix == "pal8")
}

func sameExt(ext, container string) bool {
	return ext == "."+container || ext == ".jpeg" && container == "jpg"
}

func (c *composer) music() {
	cover := false
	for _, v := range c.info.Video {
		cover = cover || v.IsAttachedPic
	}
	if name := tokenValue(c.spec["a"]); name != "" && name != "copy" {
		if p, err := c.l.Get(name); err == nil && !sameExt(c.ext, p.Container) {
			c.problem("the output %s does not match %s; name the output .%s", c.ext, name, p.Container)
		}
	}
	dropCover := func() {
		if len(c.p.Tracks) == 0 {
			c.p.Tracks = []string{"all"}
		}
		c.p.Tracks = append(c.p.Tracks, "-v")
	}
	if c.spec["exif"] != "" {
		c.p.Args = append(c.p.Args, "-map_metadata", "-1")
		if cover {
			dropCover()
			c.line("meta", "tags, cover → remove")
		} else {
			c.line("meta", "tags → remove")
		}
		return
	}
	switch {
	case !cover:
		c.line("meta", "tags: as is")
	case slices.Contains(coverTagExts, c.ext):
		dropCover() // goes along as a Vorbis comment instead
		c.p.CoverTag = true
		c.line("meta", "tags, cover: as is")
	case slices.Contains(coverStreamExts, c.ext):
		c.line("meta", "tags, cover: as is")
	default:
		c.problem("cover art does not fit in %s: remove it with -exif, or pick opus, aac, mp3 or flac", strings.TrimPrefix(c.ext, "."))
	}
}
