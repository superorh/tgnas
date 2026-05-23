package telegram

import "testing"

func TestCaptionTemplateRendersAllVariables(t *testing.T) {
	tpl, err := ParseCaptionTemplate("bucket={bucket} key={key} name={name} size={size} bytes={bytes} part={part} parts={parts} chunk={chunk}")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}

	caption := tpl.Render(CaptionData{
		Bucket: "photos",
		Key:    "path/hello.txt",
		Name:   "hello.txt",
		Size:   "5 B",
		Bytes:  5,
		Part:   2,
		Parts:  3,
	})

	want := "bucket=photos key=path/hello.txt name=hello.txt size=5 B bytes=5 part=2 parts=3 chunk=2/3"
	if caption != want {
		t.Fatalf("caption = %q, want %q", caption, want)
	}
}

func TestCaptionTemplateNonChunkedChunkIsEmpty(t *testing.T) {
	tpl, err := ParseCaptionTemplate("part={part} parts={parts} chunk={chunk}")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}
	caption := tpl.Render(CaptionData{Part: 1, Parts: 1})
	if caption != "part=1 parts=1 chunk=" {
		t.Fatalf("caption = %q", caption)
	}
}

func TestCaptionTemplateRejectsUnknownVariable(t *testing.T) {
	_, err := ParseCaptionTemplate("{unknown}")
	if err == nil {
		t.Fatal("ParseCaptionTemplate returned nil error")
	}
}

func TestCaptionTemplateTruncatesToLimit(t *testing.T) {
	tpl, err := ParseCaptionTemplate("{name}")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}

	caption := tpl.RenderWithLimit(CaptionData{Name: "你好世界"}, 3)
	if caption != "你好世" {
		t.Fatalf("caption = %q, want %q", caption, "你好世")
	}
}

func TestCaptionTemplateEmptyTemplateRendersEmptyCaption(t *testing.T) {
	tpl, err := ParseCaptionTemplate("")
	if err != nil {
		t.Fatalf("ParseCaptionTemplate returned error: %v", err)
	}
	caption := tpl.Render(CaptionData{Bucket: "photos", Key: "hello.txt", Name: "hello.txt", Bytes: 5, Part: 1, Parts: 1})
	if caption != "" {
		t.Fatalf("caption = %q, want empty", caption)
	}
}
