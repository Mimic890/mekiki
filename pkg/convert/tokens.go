package convert

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Media types of the token verbs, as in Preset.Media.
const (
	MediaVideo = "video"
	MediaPhoto = "image"
	MediaMusic = "audio"
)

// Spec maps a slot, the one thing a token changes, to the token that named it.
// Slots: v (codec), q, size, fps, gpu, s, exif, a and a.br (all audio tracks),
// a:LANG and a:LANG.br (one language).
type Spec map[string]string

var (
	videoCodecs = map[string]string{"h265": "hevc", "hevc": "hevc", "h264": "h264", "av1": "av1"} // token to built-in preset
	photoCodecs = map[string]string{"webp": "webp", "jpeg": "jpeg", "jpg": "jpeg", "avif": "avif", "png": "png"}
	audioCodecs = map[string]string{"opus": "libopus", "aac": "aac", "mp3": "libmp3lame", "flac": "flac", "ac3": "ac3", "eac3": "eac3", "copy": "copy"} // token to encoder

	reQuality = regexp.MustCompile(`^(q\d+|good|best)$`)
	reSize    = regexp.MustCompile(`^[1-9]\d*(p|px|%)$`)
	reFPS     = regexp.MustCompile(`^[1-9]\d*(\.\d+)?fps$`)
	reBitrate = regexp.MustCompile(`^[1-9]\d*k$`)
	reAudio   = regexp.MustCompile(`^a:(?:([a-z]{2,3})=)?([a-z0-9]+)$`)
	reDrop    = regexp.MustCompile(`^-a:([a-z]{2,3})$`)
)

var errNotToken = errors.New("not a token")

// IsToken reports whether arg has the form of a token of any verb, as opposed to a file name.
func IsToken(arg string) bool {
	_, err := tokenSlot(MediaVideo, arg)
	return !errors.Is(err, errNotToken)
}

func verbName(media string) string {
	return map[string]string{MediaVideo: "video", MediaPhoto: "photo", MediaMusic: "music"}[media]
}

// tokenSlot returns the slot a token sets. errNotToken means arg has no token
// form at all; any other error is a token that makes no sense for this verb.
func tokenSlot(media, tok string) (string, error) {
	only := func(slot, hint string, allowed ...string) (string, error) {
		if slices.Contains(allowed, media) {
			return slot, nil
		}
		if hint != "" {
			hint = "; write " + hint
		}
		return "", fmt.Errorf("%s does not apply to mekiki %s%s", tok, verbName(media), hint)
	}
	switch {
	case reQuality.MatchString(tok):
		return "q", nil
	case reSize.MatchString(tok):
		return only("size", "", MediaVideo, MediaPhoto)
	case reFPS.MatchString(tok):
		return only("fps", "", MediaVideo)
	case tok == "gpu":
		return only("gpu", "", MediaVideo)
	case tok == "-s":
		return only("s", "", MediaVideo)
	case tok == "-exif":
		return "exif", nil
	case reDrop.MatchString(tok):
		return only("a:"+reDrop.FindStringSubmatch(tok)[1], "", MediaVideo, MediaMusic)
	case strings.HasPrefix(tok, "a:"):
		m := reAudio.FindStringSubmatch(tok)
		if m == nil {
			return "", fmt.Errorf("cannot read %s: write a:CODEC, a:192k, a:LANG=CODEC or a:LANG=192k", tok)
		}
		slot := "a"
		if m[1] != "" {
			slot = "a:" + m[1]
		}
		switch {
		case reBitrate.MatchString(m[2]):
			slot += ".br"
		case audioCodecs[m[2]] == "":
			return "", fmt.Errorf("unknown audio codec in %s: use %s", tok, strings.Join(sortedKeys(audioCodecs), ", "))
		}
		return only(slot, "", MediaVideo, MediaMusic)
	case videoCodecs[tok] != "":
		return only("v", "", MediaVideo)
	case photoCodecs[tok] != "":
		return only("v", "", MediaPhoto)
	case audioCodecs[tok] != "":
		return only("a", "a:"+tok, MediaMusic)
	case reBitrate.MatchString(tok):
		return only("a.br", "a:"+tok, MediaMusic)
	}
	return "", errNotToken
}

// ParseSpec reads tokens and the names of token presets. Two tokens for one
// slot conflict, unless one comes from a preset: then the manual token wins.
func (l *Library) ParseSpec(media string, tokens []string) (Spec, error) {
	fromPresets, manual := Spec{}, Spec{}
	for _, tok := range tokens {
		slot, err := tokenSlot(media, tok)
		if err == nil {
			if err := manual.add(slot, tok); err != nil {
				return nil, err
			}
			continue
		}
		if !errors.Is(err, errNotToken) {
			return nil, err
		}
		p, err := l.Get(tok)
		if err != nil {
			return nil, unknownToken(tok)
		}
		if len(p.Tokens) == 0 {
			return nil, fmt.Errorf("preset %s has no tokens; use it with mekiki convert --preset %s", tok, tok)
		}
		if p.Media != media {
			return nil, fmt.Errorf("preset %s is for %s files, not %s", tok, p.Media, verbName(media))
		}
		for _, saved := range p.Tokens {
			slot, err := tokenSlot(media, saved)
			if err != nil {
				return nil, fmt.Errorf("preset %s: token %s: %w", tok, saved, err)
			}
			if err := fromPresets.add(slot, saved); err != nil {
				return nil, err
			}
		}
	}
	for slot, tok := range manual {
		fromPresets[slot] = tok
	}
	return fromPresets, fromPresets.check(media)
}

func (s Spec) add(slot, tok string) error {
	if old, ok := s[slot]; ok && old != tok {
		return fmt.Errorf("%s and %s conflict: both set the same thing, keep one", old, tok)
	}
	s[slot] = tok
	return nil
}

func (s Spec) check(media string) error {
	for slot, tok := range s {
		base, isRate := strings.CutSuffix(slot, ".br")
		if !isRate {
			continue
		}
		if other := s[base]; strings.HasPrefix(other, "-") || tokenValue(other) == "copy" {
			return fmt.Errorf("%s and %s conflict: the track is not encoded, so it has no bitrate", other, tok)
		}
		if media == MediaMusic && s["q"] != "" {
			return fmt.Errorf("%s and %s conflict: use a quality or a bitrate, not both", s["q"], tok)
		}
	}
	return nil
}

// Tokens lists the spec as tokens in a stable order, for saving as a preset.
func (s Spec) Tokens() []string {
	list := make([]string, 0, len(s))
	for _, slot := range sortedKeys(s) {
		list = append(list, s[slot])
	}
	return list
}

// tokenValue strips the a: and a:LANG= prefixes.
func tokenValue(tok string) string { return tok[strings.LastIndexAny(tok, ":=")+1:] }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unknownToken(tok string) error {
	if strings.ContainsAny(tok, "./") {
		return fmt.Errorf("%s: no such file", tok)
	}
	var best []string
	bestDist := min(2, len(tok)/2) // short tokens are near everything
	known := []string{"good", "best", "gpu", "-s", "-exif"}
	for _, m := range []map[string]string{videoCodecs, photoCodecs, audioCodecs} {
		known = append(known, sortedKeys(m)...)
	}
	slices.Sort(known)
	for _, word := range slices.Compact(known) {
		switch d := editDistance(tok, word); {
		case d < bestDist:
			best, bestDist = []string{word}, d
		case d == bestDist:
			best = append(best, word)
		}
	}
	switch {
	case len(best) > 0:
		return fmt.Errorf("unknown token %s: did you mean %s?", tok, strings.Join(best, " or "))
	case strings.Trim(tok, "0123456789") == "":
		return fmt.Errorf("unknown token %s: a bare number means nothing; write 1080p, 30fps, q21 or 160k", tok)
	case strings.HasPrefix(tok, "-"):
		return fmt.Errorf("unknown token %s: removal tokens are -a:LANG, -s and -exif", tok)
	}
	return fmt.Errorf("%s is neither a file nor a token or preset; see -h for the tokens", tok)
}

func editDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		diag := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			diag, row[j] = row[j], min(row[j]+1, row[j-1]+1, diag+cost)
		}
	}
	return row[len(b)]
}
