// Package tools locates the external programs mekiki depends on
// and reports their versions.
package tools

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Known lists every external tool mekiki can use, in display order.
var Known = []string{"ffprobe", "ffmpeg", "mkvpropedit", "exiftool"}

// products names what provides each tool.
var products = map[string]string{
	"ffprobe":     "FFmpeg",
	"ffmpeg":      "FFmpeg",
	"mkvpropedit": "MKVToolNix",
	"exiftool":    "ExifTool",
}

// managers holds the package managers to look for, in order, and each product's package.
var managers = []struct {
	goos, probe, command string
	packages             map[string]string
}{
	{"linux", "apt-get", "sudo apt install", map[string]string{"FFmpeg": "ffmpeg", "MKVToolNix": "mkvtoolnix", "ExifTool": "libimage-exiftool-perl"}},
	{"linux", "dnf", "sudo dnf install", map[string]string{"FFmpeg": "ffmpeg (from RPM Fusion)", "MKVToolNix": "mkvtoolnix", "ExifTool": "perl-Image-ExifTool"}},
	{"linux", "pacman", "sudo pacman -S", map[string]string{"FFmpeg": "ffmpeg", "MKVToolNix": "mkvtoolnix-cli", "ExifTool": "perl-image-exiftool"}},
	{"darwin", "brew", "brew install", map[string]string{"FFmpeg": "ffmpeg", "MKVToolNix": "mkvtoolnix", "ExifTool": "exiftool"}},
	{"windows", "winget", "winget install", map[string]string{"FFmpeg": "Gyan.FFmpeg", "MKVToolNix": "MoritzBunkus.MKVToolNix", "ExifTool": "OliverBetz.ExifTool"}},
}

// installHint tells how to install a tool on goos, given which programs exist.
func installHint(name, goos string, exists func(string) bool) string {
	product := products[name]
	if product == "" {
		return ""
	}
	for _, m := range managers {
		if m.goos == goos && exists(m.probe) {
			return "install " + product + ": " + m.command + " " + m.packages[product]
		}
	}
	return "install " + product + ", see https://github.com/Mimic890/mekiki#install"
}

func inPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

var versionArgs = map[string]string{
	"ffprobe":     "-version",
	"ffmpeg":      "-version",
	"mkvpropedit": "--version",
	"exiftool":    "-ver",
}

// ErrMissing matches any MissingError via errors.Is.
var ErrMissing = errors.New("required tool not found")

// MissingError reports that a required external tool is not installed.
type MissingError struct{ Name string }

// Error names the tool and how to install it.
func (e *MissingError) Error() string {
	msg := e.Name + " not found in PATH"
	if hint := installHint(e.Name, runtime.GOOS, inPath); hint != "" {
		msg += "; " + hint
	}
	return msg
}

// Is makes errors.Is(err, ErrMissing) true for every MissingError.
func (e *MissingError) Is(target error) bool { return target == ErrMissing }

// Path returns the absolute path of a tool or a *MissingError.
func Path(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", &MissingError{Name: name}
	}
	return path, nil
}

// Status describes one external tool as found on this machine.
type Status struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Check locates a tool and asks it for its version.
func Check(ctx context.Context, name string) Status {
	st := Status{Name: name}
	path, err := Path(name)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Found, st.Path = true, path

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, versionArgs[name]).Output()
	if err != nil {
		st.Error = "version check failed: " + err.Error()
		return st
	}
	st.Version = parseVersion(string(out))
	return st
}

// parseVersion extracts the version from the first line of a -version banner.
func parseVersion(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	f := strings.Fields(line)
	switch {
	case len(f) >= 3 && f[1] == "version": // ffmpeg, ffprobe
		return f[2]
	case len(f) >= 2 && strings.HasPrefix(f[1], "v"): // mkvpropedit
		return strings.TrimPrefix(f[1], "v")
	}
	return strings.TrimSpace(line) // exiftool prints the bare version
}
