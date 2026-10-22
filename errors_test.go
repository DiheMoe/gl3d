package gl3d

import (
	"errors"
	"testing"
)

func TestErrorTaxonomy(t *testing.T) {
	var noAudio error = ErrNoAudio
	if !errors.Is(noAudio, ErrNoAudio) {
		t.Fatal("ErrNoAudio should match itself")
	}

	var unsup error = &UnsupportedError{Feature: "Layer II"}
	var u *UnsupportedError
	if !errors.As(unsup, &u) || u.Feature != "Layer II" {
		t.Fatalf("UnsupportedError: %v", unsup)
	}

	var trunc error = &TruncatedError{Offset: 1234, What: "frame body"}
	var tr *TruncatedError
	if !errors.As(trunc, &tr) || tr.Offset != 1234 || tr.What != "frame body" {
		t.Fatalf("TruncatedError: %v", trunc)
	}

	var corrupt error = &CorruptError{Frame: 42, Detail: "CRC mismatch"}
	var c *CorruptError
	if !errors.As(corrupt, &c) || c.Frame != 42 {
		t.Fatalf("CorruptError: %v", corrupt)
	}

	var limit error = &LimitError{Budget: "tag skip"}
	var l *LimitError
	if !errors.As(limit, &l) || l.Budget != "tag skip" {
		t.Fatalf("LimitError: %v", limit)
	}

	// 不同错误类别之间不得互相匹配,调用方才能用 errors.As 区分
	if errors.As(unsup, &c) || errors.As(trunc, &u) {
		t.Fatal("error categories must be distinct")
	}
}

func TestErrorMessages(t *testing.T) {
	checks := []struct {
		err  error
		want string
	}{
		{ErrNoAudio, "gl3d: no audio data"},
		{&UnsupportedError{Feature: "free format bitrate"}, "free format"},
		{&TruncatedError{Offset: 100, What: "frame header"}, "100"},
		{&CorruptError{Frame: 7, Detail: "bad huffman"}, "7"},
		{&LimitError{Budget: "sync search"}, "sync search"},
	}
	for _, tc := range checks {
		if got := tc.err.Error(); !contains(got, tc.want) {
			t.Fatalf("%q should contain %q", got, tc.want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
