package store

import (
	"strconv"
	"strings"
)

type ByteRange struct {
	Start int64
	End   int64
}

func (r ByteRange) Length() int64 {
	if r.Start < 0 || r.End < r.Start {
		return 0
	}
	return r.End - r.Start + 1
}

func ParseRange(header string, size int64) (ByteRange, error) {
	if header == "" {
		if size <= 0 {
			return ByteRange{}, ErrInvalidRange
		}
		return ByteRange{Start: 0, End: size - 1}, nil
	}
	if size <= 0 {
		return ByteRange{}, ErrInvalidRange
	}
	if !strings.HasPrefix(header, "bytes=") {
		return ByteRange{}, ErrInvalidRange
	}

	spec := strings.TrimPrefix(header, "bytes=")
	if spec == "" || strings.Contains(spec, ",") {
		return ByteRange{}, ErrInvalidRange
	}

	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return ByteRange{}, ErrInvalidRange
	}

	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return ByteRange{}, ErrInvalidRange
		}
		if suffix > size {
			suffix = size
		}
		return ByteRange{Start: size - suffix, End: size - 1}, nil
	}

	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return ByteRange{}, ErrInvalidRange
	}

	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return ByteRange{}, ErrInvalidRange
		}
		if end >= size {
			end = size - 1
		}
	}

	return ByteRange{Start: start, End: end}, nil
}

type ChunkRef struct {
	Part   int
	FileID string
	Offset int64
	Size   int64
}

type SelectedChunk struct {
	ChunkRef
	Skip int64
	Take int64
}

func SelectChunksForRange(chunks []ChunkRef, r ByteRange) []SelectedChunk {
	selected := make([]SelectedChunk, 0, len(chunks))
	for _, chunk := range chunks {
		chunkStart := chunk.Offset
		chunkEnd := chunk.Offset + chunk.Size - 1
		if chunk.Size <= 0 || chunkEnd < r.Start || chunkStart > r.End {
			continue
		}

		overlapStart := maxInt64(chunkStart, r.Start)
		overlapEnd := minInt64(chunkEnd, r.End)
		selected = append(selected, SelectedChunk{
			ChunkRef: chunk,
			Skip:     overlapStart - chunkStart,
			Take:     overlapEnd - overlapStart + 1,
		})
	}
	return selected
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
