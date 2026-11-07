package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Shell completion: each script hands the command line to the hidden
// "mekiki __complete" and adds file names itself, so the lists below are the
// only place that knows the command line.

var completionScripts = map[string]string{
	"bash": `# bash completion for mekiki. Load it with: source <(mekiki completion bash)
_mekiki() {
	local line=${COMP_LINE:0:COMP_POINT} word IFS=$'\n'
	local -a args
	IFS=$' \t' read -ra args <<<"$line"
	[[ $line == *[[:space:]] ]] && args+=("")
	word=${args[${#args[@]}-1]}
	COMPREPLY=($(compgen -W "$(mekiki __complete "${args[@]}" 2>/dev/null)" -- "$word"))
	if [[ $word == *:* ]]; then
		COMPREPLY=("${COMPREPLY[@]#"${word%:*}:"}") # bash replaces only the part after the last ":"
	else
		COMPREPLY+=($(compgen -f -- "$word"))
	fi
}
complete -o filenames -F _mekiki mekiki
`,
	"zsh": `#compdef mekiki
# zsh completion for mekiki. Load it after compinit with: source <(mekiki completion zsh)
_mekiki() {
	local -a c
	c=(${(f)"$(mekiki __complete "${(@)words[1,CURRENT]}" 2>/dev/null)"})
	compadd -a c
	_files
}
compdef _mekiki mekiki
`,
	"fish": `# fish completion for mekiki. Load it with: mekiki completion fish | source
complete -c mekiki -a '(mekiki __complete (commandline -opc) (commandline -ct) 2>/dev/null)'
`,
}

var (
	commands    = []string{"info", "shrink", "video", "photo", "music", "convert", "preset", "meta", "doctor", "tui", "completion", "version", "help"}
	subcommands = map[string][]string{"preset": {"list", "show", "save", "rm", "edit"}, "meta": {"get", "set", "rm"}, "completion": {"bash", "zsh", "fish"}}
	// tokenWords are offered to video, photo and music, each keeping the ones ParseSpec takes.
	tokenWords = []string{"h265", "h264", "av1", "webp", "jpeg", "avif", "png", "opus", "aac", "mp3", "flac",
		"good", "best", "1080p", "720p", "2160p", "2000px", "50%", "30fps", "gpu", "a:opus", "a:aac", "a:flac", "a:192k",
		"128k", "160k", "192k", "256k", "320k", "-s", "-exif"}
)

func (a *app) completion(args []string) error {
	if len(args) != 1 || completionScripts[args[0]] == "" {
		return usageErr("completion: expected one of bash, zsh or fish")
	}
	fmt.Fprint(a.stdout, completionScripts[args[0]])
	return nil
}

// complete returns the candidates for the last of words, which hold the command
// line up to the cursor, program name first. Shells filter them by prefix.
func (a *app) complete(ctx context.Context, words []string) []string {
	if len(words) < 2 {
		return nil
	}
	cur, prev := words[len(words)-1], words[1:len(words)-1]
	var list []string
	switch {
	case len(prev) == 0:
		list = append(flagNames(commandFlags(ctx, nil)), commands...) // mekiki -r is info -r
	case subcommands[prev[0]] != nil && len(prev) == 1:
		list = subcommands[prev[0]]
	default:
		cmd, path := prev[0], prev[:0] // no command: mekiki <path...> is info
		if slices.Contains(commands, cmd) {
			path = prev[:1]
			if subcommands[cmd] != nil {
				path = prev[:2]
			}
		}
		fs := commandFlags(ctx, path)
		if fs == nil {
			return nil
		}
		last := prev[len(prev)-1]
		if f := fs.Lookup(strings.TrimLeft(last, "-")); len(prev) > len(path) && strings.HasPrefix(last, "-") && f != nil {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				if f.Name == "preset" {
					return presetsFor(cmd, "")
				}
				return nil // a value the shell completes as a file name, if at all
			}
		}
		list = flagNames(fs)
		switch {
		case verbMedia[cmd] != "":
			list = append(list, presetsFor(cmd, verbMedia[cmd])...)
		case cmd == "preset" && (prev[1] == "show" || prev[1] == "rm"):
			list = append(list, presetsFor(cmd, "")...)
		}
	}
	// Flags only once a "-" is typed; video's -s and -exif are dash tokens too.
	list = slices.DeleteFunc(slices.Clone(list), func(c string) bool { return strings.HasPrefix(c, "-") != strings.HasPrefix(cur, "-") })
	slices.Sort(list)
	return slices.Compact(list) // a preset may share a token's name
}

// flagNames lists the flags of fs as typed: -o, --overwrite.
func flagNames(fs *flag.FlagSet) []string {
	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		names = append(names, map[bool]string{true: "-", false: "--"}[len(f.Name) == 1]+f.Name)
	})
	return names
}

// commandFlags returns the flags of the command named by path, which parseFlags
// hands over instead of running it. Nil means the command has no flag set.
func commandFlags(ctx context.Context, path []string) *flag.FlagSet {
	probe := &app{stdout: io.Discard, stderr: io.Discard, onlyFlags: true}
	probe.run(ctx, path)
	return probe.flags
}

// presetsFor lists the presets that fit where they are typed: tokens and token
// presets of media for a verb, presets without tokens for --preset, all otherwise.
func presetsFor(cmd, media string) []string {
	lib, err := loadLibrary()
	if err != nil {
		return nil
	}
	var names []string
	if media != "" {
		for _, tok := range append(slices.Clone(tokenWords), lib.Names()...) {
			if _, err := lib.ParseSpec(media, []string{tok}); err == nil {
				names = append(names, tok)
			}
		}
		return names
	}
	for _, name := range lib.Names() {
		if p, err := lib.Get(name); err == nil && (cmd != "convert" || len(p.Tokens) == 0) {
			names = append(names, name)
		}
	}
	return names
}
