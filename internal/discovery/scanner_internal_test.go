package discovery

import (
	"testing"
	"time"
)

func TestParseLinuxProcStatStartTicks(t *testing.T) {
	stat := "12345 (claude) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20"
	ticks, err := parseLinuxProcStatStartTicks(stat)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if ticks != 424242 {
		t.Fatalf("expected 424242 ticks, got %d", ticks)
	}
}

func TestParseDarwinLStart(t *testing.T) {
	got, err := parseDarwinLStart("Wed Mar 27 12:34:56 2024")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	want := time.Date(2024, time.March, 27, 12, 34, 56, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}
