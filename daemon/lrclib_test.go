package daemon

import "testing"

// The timed format, as it comes in the wild: minutes, seconds and hundredths,
// sometimes thousandths, sometimes several stamps for one line.
func TestReadingTheTimedFormat(t *testing.T) {
	lines := parseLRC("[ar: Hip Hop Boyz]\n[00:00.11] A hétvégén\n[01:03.5] Refrén\n[02:00.123] Vége\n")

	if len(lines) != 3 {
		t.Fatalf("read %d lines, want the three with stamps and not the header", len(lines))
	}
	for i, want := range []int64{110, 63500, 120123} {
		if lines[i].At != want {
			t.Errorf("line %d is at %dms, want %dms", i, lines[i].At, want)
		}
	}
	if lines[0].Words != "A hétvégén" {
		t.Errorf("line 0 reads %q", lines[0].Words)
	}
}

// One line sung twice carries two stamps, and is two lines to follow.
func TestALineWithTwoStamps(t *testing.T) {
	lines := parseLRC("[00:10.00][01:10.00] A hegyekbe fönn")
	if len(lines) != 2 || lines[0].At != 10_000 || lines[1].At != 70_000 {
		t.Errorf("parseLRC() = %+v, want the line at both times", lines)
	}
}

// A stamp with no words is a pause, and the screen draws it as the gap it is.
func TestAStampWithNoWords(t *testing.T) {
	lines := parseLRC("[00:05.00]\n[00:07.00] Words")
	if len(lines) != 2 || lines[0].Words != "" {
		t.Errorf("parseLRC() = %+v, want the pause kept", lines)
	}
}

// An instrumental has no words to show, and saying so is better than searching
// on until something else matches by name.
func TestAnInstrumentalHasNothingToShow(t *testing.T) {
	if got := lrclibLyrics(&lrclibRecord{Instrumental: true, PlainLyrics: "x"}); got != nil {
		t.Errorf("lrclibLyrics() = %+v, want nothing", got)
	}
}

// Timed lines win over plain ones from the same record.
func TestTimedLinesWin(t *testing.T) {
	got := lrclibLyrics(&lrclibRecord{
		PlainLyrics:  "one\ntwo",
		SyncedLyrics: "[00:01.00] one\n[00:02.00] two",
	})
	if got == nil || !got.Synced || len(got.Lines) != 2 || got.Lines[1].At != 2000 {
		t.Errorf("lrclibLyrics() = %+v, want the timed lines", got)
	}
}
