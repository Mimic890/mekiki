#!/usr/bin/env bash
# Regenerates the README screenshots in docs/img from real runs on generated media.
#
# Needs ffmpeg (with libx264 and libx265), tmux, freeze
# (go install github.com/charmbracelet/freeze@latest) and Chrome or Chromium,
# which turns freeze's SVG into PNG. Set CHROME when it is not on PATH.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
img=$repo/docs/img
work=$(mktemp -d)
media=$work/home/Media
tm() { tmux -L mekiki-shots -f /dev/null "$@"; }
trap 'tm kill-server 2>/dev/null || true; rm -rf "$work"' EXIT
trap 'exit 130' INT TERM # run the EXIT cleanup on signals too

chrome=${CHROME:-$(command -v chromium || command -v chromium-browser || command -v google-chrome || true)}
if [ -z "$chrome" ]; then
	echo "set CHROME to a Chrome or Chromium binary" >&2
	exit 1
fi

go build -o "$work/bin/mekiki" "$repo"
mkdir -p "$media/Album" "$media/Trip" "$work/src" "$img"
cd "$work/src"

# A film with two 5.1 dubs, two subtitle languages, chapters and a title.
printf '1\n00:00:01,000 --> 00:00:04,000\nДоброе утро.\n' >ru.srt
printf '1\n00:00:01,000 --> 00:00:04,000\nGood morning.\n' >en.srt
printf ';FFMETADATA1\ntitle=Holiday 2026\n' >meta.txt
for i in 0 1 2; do
	printf '[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=Part %d\n' $((i * 4000)) $((i * 4000 + 4000)) $((i + 1)) >>meta.txt
done
ffmpeg -v error -f lavfi -i "testsrc2=size=1920x1080:rate=24000/1001:duration=12" \
	-f lavfi -i "aevalsrc=0.2*sin(220*2*PI*t):c=5.1(side):s=48000:d=12" \
	-f lavfi -i "aevalsrc=0.2*sin(330*2*PI*t):c=5.1(side):s=48000:d=12" \
	-i ru.srt -i en.srt -i meta.txt -map 0 -map 1 -map 2 -map 3 -map 4 -map_metadata 5 -map_chapters 5 \
	-c:v libx264 -preset veryfast -crf 16 -c:a ac3 -b:a 448k -c:s srt \
	-metadata:s:a:0 language=rus -metadata:s:a:1 language=eng \
	-metadata:s:s:0 language=rus -metadata:s:s:1 language=eng "$media/Holiday.2026.mkv"

# A 4K HDR10 clip: 10-bit HEVC with mastering display metadata.
ffmpeg -v error -f lavfi -i "testsrc2=size=3840x2160:rate=24:duration=4" \
	-f lavfi -i "aevalsrc=0.2*sin(440*2*PI*t):c=stereo:d=4" \
	-c:v libx265 -preset ultrafast -pix_fmt yuv420p10le \
	-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc \
	-x265-params "log-level=error:hdr10=1:max-cll=1000,400:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)" \
	-c:a libopus -metadata:s:a:0 language=eng "$media/Aurora.HDR.mkv"

# An album with cover art, and photos.
ffmpeg -v error -f lavfi -i "gradients=s=600x600:c0=0x1d3557:c1=0xe76f51:d=1" -frames:v 1 cover.jpg
n=0
for song in Morning Platform Sea; do
	n=$((n + 1))
	ffmpeg -v error -f lavfi -i "aevalsrc=0.3*sin($((200 + n * 60))*2*PI*t)*exp(-mod(t\,2)):c=stereo:s=44100:d=40" \
		-i cover.jpg -map 0 -map 1 -c:a flac -sample_fmt s16 -c:v copy -disposition:v attached_pic \
		-metadata title="$song" -metadata artist="Night Trains" -metadata album=Holiday -metadata track=$n \
		"$media/Album/0$n $song.flac"
done
for i in 0412 0413 0414; do
	ffmpeg -v error -f lavfi -i "gradients=s=4032x3024:seed=$i:d=1" -frames:v 1 -q:v 2 "$media/Trip/IMG_$i.jpg"
done

# tmux is the terminal: a fresh HOME keeps user presets out and shows paths as ~.
# stty tab3 stops the TUI renderer from moving the cursor with tabs, which tmux keeps in captures.
env=(env "HOME=$work/home" "PATH=$work/bin:$PATH" TERM=xterm-256color COLORTERM=truecolor)
: >"$work/prompts"
tm new-session -d -s cli -x 100 -y 46 -c "$media" "${env[@]}" 'PS1=$ ' \
	"PROMPT_COMMAND=echo >>'$work/prompts'" bash --norc --noprofile
tm set -g status off

# settle waits up to a minute for a check to pass.
settle() {
	for _ in $(seq 600); do
		"$@" && return
		sleep 0.1
	done
	echo "timed out: $*" >&2
	exit 1
}
prompted() { [ "$(wc -l <"$work/prompts")" -gt "$1" ]; }
showing() { tm capture-pane -t "$1" -p | grep -q "$2"; }

# run types a command at the prompt and waits for the next prompt.
run() {
	local seen
	seen=$(wc -l <"$work/prompts")
	tm send-keys -t cli -l "$*"
	tm send-keys -t cli Enter
	settle prompted "$seen"
}

# shot renders the visible pane of a tmux session to docs/img/NAME.png.
shot() {
	local session=$1 name=$2
	# Drop the trailing empty lines and the idle prompt.
	tm capture-pane -t "$session" -e -p | awk '{ line[NR] = $0 } END {
		n = NR
		while (n > 0) { s = line[n]; gsub(/\033\[[0-9;:]*m/, "", s); if (s !~ /^\$? *$/) break; n-- }
		for (i = 1; i <= n; i++) print line[i]
	}' >"$work/$name.ansi"
	timeout 60 freeze --execute "cat $work/$name.ansi" -o "$work/$name.svg" --window --padding 20,24 --margin 0 --font.size 14
	set -- $(grep -o '<svg width="[0-9.]*" height="[0-9.]*"' "$work/$name.svg" | grep -o '[0-9][0-9.]*')
	timeout 60 "$chrome" --headless --no-sandbox --hide-scrollbars --force-device-scale-factor=2 \
		--default-background-color=00000000 --window-size="$(printf %.0f "$1"),$(printf %.0f "$2")" \
		--screenshot="$img/$name.png" "file://$work/$name.svg" 2>/dev/null
}

settle prompted 0
run clear
run mekiki video Holiday.2026.mkv h265 q21 a:rus=opus -a:eng
run mekiki converted/Holiday.2026.mkv
shot cli video

run clear
run mekiki video Holiday.2026.mkv h256
run mekiki video Holiday.2026.mkv a:flac -o Holiday.mp4
run mekiki music Album opus 160k --dry-run
shot cli plan

tm new-session -d -s tui -x 110 -y 26 -c "$media" sh -c 'stty tab3 && exec "$@"' sh "${env[@]}" mekiki tui .
settle showing tui Holiday.2026.mkv
tm send-keys -t tui Down Down Down Down Down # past ../, Album/, Trip/, converted/ and Aurora
settle showing tui chapters
shot tui tui

echo "wrote $img"
