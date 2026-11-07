package probe_test

import (
	"fmt"
	"log"

	"github.com/Mimic890/mekiki/pkg/probe"
)

func ExampleScanFile() {
	info, err := probe.ScanFile("film.mkv")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(info.Format, info.DurationMs/1000, "s")
	for _, v := range info.Video {
		if v.IsAttachedPic {
			continue // cover art
		}
		fmt.Println(v.Codec, v.Width, v.Height, v.BitDepth, "bit HDR10:", v.HDR.IsHDR10)
	}
	for _, a := range info.Audio {
		fmt.Println(a.Language, a.Codec, a.ChannelLayout)
	}
}

func ExampleIsMediaFile() {
	fmt.Println(probe.IsMediaFile("film.MKV"), probe.IsMediaFile("notes.txt"))
	// Output: true false
}
