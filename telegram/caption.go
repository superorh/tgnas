package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const CaptionLimit = 1024

type CaptionTemplate struct {
	raw string
}

type CaptionData struct {
	Bucket string
	Key    string
	Name   string
	Size   string
	Bytes  int64
	Part   int
	Parts  int
}

var captionVariables = map[string]struct{}{
	"bucket": {},
	"key":    {},
	"name":   {},
	"size":   {},
	"bytes":  {},
	"part":   {},
	"parts":  {},
	"chunk":  {},
}

func ParseCaptionTemplate(raw string) (*CaptionTemplate, error) {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}

		end := strings.IndexByte(raw[i:], '}')
		if end < 0 {
			continue
		}
		end += i

		name := raw[i+1 : end]
		if name == "" {
			continue
		}
		if _, ok := captionVariables[name]; !ok {
			return nil, fmt.Errorf("unknown caption variable %q", name)
		}
		i = end
	}

	return &CaptionTemplate{raw: raw}, nil
}

func (t *CaptionTemplate) Render(data CaptionData) string {
	return t.RenderWithLimit(data, CaptionLimit)
}

func (t *CaptionTemplate) RenderWithLimit(data CaptionData, limit int) string {
	if t == nil || t.raw == "" || limit == 0 {
		return ""
	}

	part, parts := normalizeParts(data.Part, data.Parts)
	chunk := ""
	if !(part == 1 && parts == 1) {
		chunk = strconv.Itoa(part) + "/" + strconv.Itoa(parts)
	}

	replacer := strings.NewReplacer(
		"{bucket}", data.Bucket,
		"{key}", data.Key,
		"{name}", data.Name,
		"{size}", data.Size,
		"{bytes}", strconv.FormatInt(data.Bytes, 10),
		"{part}", strconv.Itoa(part),
		"{parts}", strconv.Itoa(parts),
		"{chunk}", chunk,
	)

	rendered := replacer.Replace(t.raw)
	if limit < 0 {
		return rendered
	}
	return truncateRunes(rendered, limit)
}

func normalizeParts(part, parts int) (int, int) {
	if part < 1 {
		part = 1
	}
	if parts < 1 {
		parts = 1
	}
	return part, parts
}

func truncateRunes(value string, limit int) string {
	if limit < 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}

	count := 0
	for i := range value {
		if count == limit {
			return value[:i]
		}
		count++
	}
	return value
}
