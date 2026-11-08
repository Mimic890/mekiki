package tools

import (
	"context"
	"errors"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := map[string]string{
		"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023\nbuilt with gcc": "6.1.1-3ubuntu5",
		"ffprobe version n7.1 Copyright (c) 2007-2024":                          "n7.1",
		"mkvpropedit v82.0 ('I'm The President') 64-bit\n":                      "82.0",
		"12.76\n": "12.76",
	}
	for out, want := range tests {
		if got := parseVersion(out); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestMissingTool(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := Path("ffprobe")
	// With an empty PATH no package manager is found either.
	if !errors.Is(err, ErrMissing) || err.Error() != "ffprobe not found in PATH; install FFmpeg, see https://github.com/Mimic890/mekiki#install" {
		t.Fatalf("Path() error = %v", err)
	}
	if st := Check(context.Background(), "ffprobe"); st.Found || st.Error == "" {
		t.Fatalf("Check() = %+v", st)
	}
}

func TestInstallHint(t *testing.T) {
	const readme = ", see https://github.com/Mimic890/mekiki#install"
	for _, tc := range []struct{ tool, goos, manager, want string }{
		{"ffprobe", "linux", "apt-get", "install FFmpeg: sudo apt install ffmpeg"},
		{"exiftool", "linux", "apt-get", "install ExifTool: sudo apt install libimage-exiftool-perl"},
		{"ffmpeg", "linux", "dnf", "install FFmpeg: sudo dnf install ffmpeg (from RPM Fusion)"},
		{"mkvpropedit", "linux", "pacman", "install MKVToolNix: sudo pacman -S mkvtoolnix-cli"},
		{"exiftool", "darwin", "brew", "install ExifTool: brew install exiftool"},
		{"ffmpeg", "windows", "winget", "install FFmpeg: winget install Gyan.FFmpeg"},
		{"ffmpeg", "linux", "zypper", "install FFmpeg" + readme},
		{"ffmpeg", "darwin", "apt-get", "install FFmpeg" + readme}, // a manager of another OS does not count
		{"unknown", "linux", "apt-get", ""},
	} {
		got := installHint(tc.tool, tc.goos, func(name string) bool { return name == tc.manager })
		if got != tc.want {
			t.Errorf("%s on %s with %s: %q, want %q", tc.tool, tc.goos, tc.manager, got, tc.want)
		}
	}
}
