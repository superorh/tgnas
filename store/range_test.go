package store

import "testing"

func TestParseRangeExact(t *testing.T) {
	r, err := ParseRange("bytes=2-5", 8)
	if err != nil {
		t.Fatalf("ParseRange returned error: %v", err)
	}
	if r.Start != 2 || r.End != 5 {
		t.Fatalf("range = %+v, want Start=2 End=5", r)
	}
	if got := r.Length(); got != 4 {
		t.Fatalf("Length = %d, want 4", got)
	}
}

func TestParseRangeOpenEnded(t *testing.T) {
	r, err := ParseRange("bytes=5-", 8)
	if err != nil {
		t.Fatalf("ParseRange returned error: %v", err)
	}
	if r.Start != 5 || r.End != 7 {
		t.Fatalf("range = %+v, want Start=5 End=7", r)
	}
}

func TestParseRangeSuffix(t *testing.T) {
	r, err := ParseRange("bytes=-3", 8)
	if err != nil {
		t.Fatalf("ParseRange returned error: %v", err)
	}
	if r.Start != 5 || r.End != 7 {
		t.Fatalf("range = %+v, want Start=5 End=7", r)
	}
}

func TestParseRangeClampsEnd(t *testing.T) {
	r, err := ParseRange("bytes=5-99", 8)
	if err != nil {
		t.Fatalf("ParseRange returned error: %v", err)
	}
	if r.Start != 5 || r.End != 7 {
		t.Fatalf("range = %+v, want Start=5 End=7", r)
	}
}

func TestParseRangeRejectsInvalidSyntax(t *testing.T) {
	cases := []string{"bytes=", "bytes=a-b", "items=1-2", "bytes=4-2", "bytes=-0", "bytes=-", "bytes=8-9"}
	for _, header := range cases {
		t.Run(header, func(t *testing.T) {
			_, err := ParseRange(header, 8)
			if err != ErrInvalidRange {
				t.Fatalf("err = %v, want %v", err, ErrInvalidRange)
			}
		})
	}
}

func TestParseRangeRejectsMultiRange(t *testing.T) {
	_, err := ParseRange("bytes=0-1,4-5", 8)
	if err != ErrInvalidRange {
		t.Fatalf("err = %v, want %v", err, ErrInvalidRange)
	}
}

func TestParseRangeRejectsZeroSizeObject(t *testing.T) {
	_, err := ParseRange("bytes=0-1", 0)
	if err != ErrInvalidRange {
		t.Fatalf("err = %v, want %v", err, ErrInvalidRange)
	}
}

func TestSelectChunksForRange(t *testing.T) {
	chunks := []ChunkRef{
		{Part: 1, FileID: "a", Offset: 0, Size: 5},
		{Part: 2, FileID: "b", Offset: 5, Size: 5},
		{Part: 3, FileID: "c", Offset: 10, Size: 5},
	}

	selected := SelectChunksForRange(chunks, ByteRange{Start: 3, End: 11})
	if len(selected) != 3 {
		t.Fatalf("len(selected) = %d, want 3", len(selected))
	}
	if selected[0].Skip != 3 {
		t.Fatalf("selected[0].Skip = %d, want 3", selected[0].Skip)
	}
	if selected[0].Take != 2 {
		t.Fatalf("selected[0].Take = %d, want 2", selected[0].Take)
	}
	if selected[1].Skip != 0 || selected[1].Take != 5 {
		t.Fatalf("selected[1] = %+v, want Skip=0 Take=5", selected[1])
	}
	if selected[2].Skip != 0 {
		t.Fatalf("selected[2].Skip = %d, want 0", selected[2].Skip)
	}
	if selected[2].Take != 2 {
		t.Fatalf("selected[2].Take = %d, want 2", selected[2].Take)
	}
}
