package main

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestComplete(t *testing.T) {
	isolateConfig(t)
	a := &app{}
	for _, tc := range []struct {
		line      string // words split on spaces; a trailing space starts an empty word
		want, not []string
	}{
		{"mekiki ", []string{"video", "meta", "completion"}, []string{"-r"}},
		{"mekiki -", []string{"-r", "--json"}, []string{"video"}},
		{"mekiki video f.mkv a:o", []string{"a:opus", "h265", "hevc"}, []string{"webp", "160k", "-o"}},
		{"mekiki video -", []string{"-o", "-r", "--dry-run", "-s", "-exif"}, []string{"h265"}},
		{"mekiki music ", []string{"opus", "160k"}, []string{"h265", "1080p"}},
		{"mekiki convert in.mkv --preset ", []string{"hevc", "remux"}, []string{"-o"}},
		{"mekiki music -o ", nil, []string{"opus", "-r"}},
		{"mekiki meta ", []string{"get", "set", "rm"}, nil},
		{"mekiki meta get -", []string{"-r", "--json"}, nil},
		{"mekiki film.mkv -", []string{"-r"}, nil},
		{"mekiki preset show ", []string{"hevc"}, nil},
		{"mekiki completion ", []string{"bash", "zsh", "fish"}, nil},
	} {
		got := a.complete(context.Background(), strings.Split(tc.line, " "))
		for _, w := range tc.want {
			if !slices.Contains(got, w) {
				t.Errorf("%q: %q missing from %v", tc.line, w, got)
			}
		}
		for _, w := range tc.not {
			if slices.Contains(got, w) {
				t.Errorf("%q: %q offered in %v", tc.line, w, got)
			}
		}
	}
	if code, _, _ := runApp(context.Background(), "completion", "tcsh"); code != exitUsage {
		t.Errorf("completion tcsh: exit %d", code)
	}
}

// TestCompletionScripts runs the bash and fish scripts against a stub mekiki
// that answers only the expected __complete call.
func TestCompletionScripts(t *testing.T) {
	chdir(t, t.TempDir()) // no file names in the answers
	if _, err := exec.LookPath("bash"); err == nil {
		script := completionScripts["bash"] + `
mekiki() { local IFS=" "; [[ "$*" == "__complete mekiki video a:o" ]] && printf '%s\n' a:opus a:aac h265; }
COMP_LINE="mekiki video a:o" COMP_POINT=16
_mekiki
printf '%s\n' "${COMPREPLY[@]}"`
		out, err := exec.Command("bash", "--norc", "-c", script).Output()
		if got := strings.TrimSpace(string(out)); err != nil || got != "opus" {
			t.Errorf("bash: %q, %v; want the part after the colon, opus", got, err)
		}
	}
	if _, err := exec.LookPath("fish"); err == nil {
		script := completionScripts["fish"] + `
function mekiki; test "$argv" = "__complete mekiki video a:o"; and printf '%s\n' a:opus a:aac h265; end
complete -C 'mekiki video a:o'`
		out, err := exec.Command("fish", "--no-config", "-c", script).Output()
		if got := strings.TrimSpace(string(out)); err != nil || got != "a:opus" {
			t.Errorf("fish: %q, %v; want a:opus", got, err)
		}
	}
}
