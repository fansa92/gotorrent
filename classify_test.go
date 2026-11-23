package main

import "testing"

func TestFileCategory(t *testing.T) {
	cases := map[string]string{
		"a.mp4":       "video",
		"b.MKV":       "video",
		"song.flac":   "audio",
		"pic.PNG":     "image",
		"doc.pdf":     "doc",
		"arch.7z":     "archive",
		"app.exe":     "app",
		"sub.srt":     "subtitle",
		"unknown.xyz": "other",
	}
	for name, want := range cases {
		if got := fileCategory(name); got != want {
			t.Errorf("fileCategory(%q)=%q,期望 %q", name, got, want)
		}
	}
}

func TestDominantCategory(t *testing.T) {
	// 视频文件最大,应判为 video
	paths := []string{"movie.mp4", "sub.srt", "nfo.nfo"}
	sizes := []int64{1000, 50, 10}
	if got := dominantCategory(paths, sizes); got != "video" {
		t.Errorf("dominantCategory=%q,期望 video", got)
	}
}

func TestSanitizePathSeg(t *testing.T) {
	cases := map[string]string{
		"movies":      "movies",
		"../etc":      "__etc",
		"a/b\\c:d":    "a_b_c_d",
		"":            "other",
		".":           "other",
		"tag with 汉字": "tag with 汉字",
	}
	for in, want := range cases {
		if got := sanitizePathSeg(in); got != want {
			t.Errorf("sanitizePathSeg(%q)=%q,期望 %q", in, got, want)
		}
	}
}
