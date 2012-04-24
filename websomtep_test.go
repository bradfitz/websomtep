package main

import (
	"crypto/sha1"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

type parseTest struct {
	File          string
	Subject, Body string
	Images        map[string]string // sha1 -> content-type
}

var parseTests = []parseTest{
	{
		File:    "plain-text",
		Subject: "plain text subject",
		Body:    "plain text body",
	},
	{
		File:    "html-msg",
		Subject: "html subject",
		Body:    "html *body*",
	},
	{
		File:    "with-image",
		Subject: "subjecto",
		Body:    "bodyo",
		Images: map[string]string{
			"87cf0355fc364349590bc7b676d92298a2065146": "image/png",
		},
	},
}

func TestMailParsing(t *testing.T) {
	for i, tt := range parseTests {
		f, err := os.Open("testdata/" + tt.File)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var m Message
		err = m.parse(f)
		if g, w := m.Subject, tt.Subject; g != w {
			t.Errorf("%d. subject = %q; want %q", i, g, w)
		}
		if g, w := strings.TrimSpace(m.Body), tt.Body; g != w {
			if len(g) > 1024 {
				g = g[:1024]
			}
			t.Errorf("%d. body = %q; want %q", i, g, w)
		}
		gotImages := map[string]string{}
		for _, im := range m.images {
			s1 := sha1.New()
			s1.Write(im.Data)
			gotImages[fmt.Sprintf("%x", s1.Sum(nil))] = im.Type
		}
		if m.images == nil && len(gotImages) == 0 {
			gotImages = nil
		}
		if !reflect.DeepEqual(tt.Images, gotImages) {
			t.Errorf("%d. got images = %+v; want %+v", i, gotImages, tt.Images)
		}
	}
}
