package meta_test

import (
	"context"
	"fmt"
	"log"

	"github.com/Mimic890/mekiki/pkg/meta"
)

func ExampleRead() {
	ctx := context.Background()
	et, err := meta.StartExifTool(ctx) // optional: nil reads only ffprobe tags
	if err != nil {
		log.Fatal(err)
	}
	defer et.Close()
	m, err := meta.Read(ctx, "photo.jpg", et)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(m.Groups["ExifIFD"]["DateTimeOriginal"])
}

// Set the title of a film and the language of its second audio track in place.
// Matroska files are edited by mkvpropedit when it is installed, others are remuxed.
func ExampleApply() {
	ctx := context.Background()
	edits := []meta.Edit{
		{Set: []meta.Tag{{Key: "title", Value: "Holiday 2026"}}},
		{Track: "a:1", Set: []meta.Tag{{Key: "language", Value: "eng"}, {Key: "default", Value: "1"}}},
	}
	for _, e := range edits {
		if err := e.Validate(); err != nil {
			log.Fatal(err)
		}
		backend, err := meta.Apply(ctx, "film.mkv", "", e, false, nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("written with", backend)
	}
}
