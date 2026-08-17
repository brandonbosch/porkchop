// Package diffview turns a reading diff (meat's Result.SmartDiff) into a
// semantic row model: a flat slice of classified lines that internal/ui renders.
//
// It is a pure transform — stdlib only, no Lip Gloss, no terminal. Every row
// carries a kind (meta/hunk/context/add/del/fold) and the exact source text of
// the line, and nothing else. All styling and layout decisions live in
// internal/ui; keeping diffview styling-free is what makes it unit-testable
// against meat/testdata goldens with a binary pass/fail signal, and keeps the
// hardest classification logic in one small, terminal-independent place.
//
// Parsing is deliberately lenient. meat documents that a reading diff's @@ hunk
// counts go stale once lines are removed, so this parser never reads those
// counts: it walks structurally, staying inside a hunk body until it hits the
// next structural boundary (a new file header or hunk header). The classifier
// is a simplified port of meat's own analyzeDiff (meat/diff.go), sharing its
// two load-bearing disambiguations: a "--- x" / "+++ y" file-header pair versus
// "-- x" / "++ y" removed/added source lines, and a machine-generated "..."
// fold row versus ordinary content.
package diffview

import "strings"

// RowKind classifies one line of a reading diff.
type RowKind uint8

const (
	// RowMeta is diff structure that is not source: "diff --git" lines, "index"
	// lines, the "--- a/x" / "+++ b/x" file-marker pair, mode/rename/copy lines,
	// and the "\ No newline at end of file" marker.
	RowMeta RowKind = iota
	// RowHunk is a hunk header ("@@ -a,b +c,d @@ ...").
	RowHunk
	// RowContext is an unchanged line carried for context (leading space).
	RowContext
	// RowAdd is an added line (leading '+').
	RowAdd
	// RowDel is a removed line (leading '-').
	RowDel
	// RowFold is a machine-generated "..." elision marker meat emits in place of
	// two or more contiguous same-polarity lines it judged noise. It keeps the
	// polarity of the lines it replaced ("+ ...", "- ...", or context " ...")
	// but is classified as a fold regardless, because it represents hidden
	// content rather than a real change — ui colors it amber.
	RowFold
)

// Row is one classified line of a reading diff. Text is the exact source of the
// line, including its leading marker byte (' ', '+', '-') and any indentation,
// with the trailing newline stripped. ui owns how the marker and content are
// presented; diffview never alters the text.
type Row struct {
	Kind RowKind
	Text string
}

// Parse classifies every line of a reading diff into a Row. It is total and
// lossless: each input line becomes exactly one Row whose Text is that line
// verbatim, so strings.Join of the rows' Text (with "\n") reconstructs the
// input (modulo a single trailing newline, which — like meat's own line
// splitter — is dropped). A malformed or empty diff yields whatever rows the
// lenient walk produces; Parse never errors and never panics.
func Parse(readingDiff string) []Row {
	lines := splitLines(readingDiff)
	rows := make([]Row, 0, len(lines))

	inHunk := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if inHunk {
			switch {
			// Structural boundaries end the hunk body. Re-classify this same
			// line through the outer (header) logic by leaving inHunk false and
			// falling through — i is not advanced past it here.
			case isGitDiffHeader(line), strings.HasPrefix(line, "@@"),
				isRawOldFileHeader(lines, i), isNoNewlineMarker(line):
				inHunk = false
			case len(line) > 0 && (line[0] == ' ' || line[0] == '+' || line[0] == '-'):
				rows = append(rows, Row{Kind: hunkLineKind(line), Text: line})
				continue
			default:
				// A blank or otherwise unrecognized line ends the hunk body,
				// matching meat: an empty string is not a valid hunk source line.
				inHunk = false
			}
		}

		// Outside a hunk body: the only source-bearing header is a hunk header;
		// everything else (file headers, index/mode/rename lines, the "---"/"+++"
		// pair, blanks, stray text) is structural metadata.
		if strings.HasPrefix(line, "@@") {
			rows = append(rows, Row{Kind: RowHunk, Text: line})
			inHunk = true
			continue
		}
		rows = append(rows, Row{Kind: RowMeta, Text: line})
	}
	return rows
}

// hunkLineKind classifies a line already known to be inside a hunk body and to
// start with a diff marker byte (' ', '+', '-').
func hunkLineKind(line string) RowKind {
	if isFixedFoldLine(line) {
		return RowFold
	}
	switch line[0] {
	case '+':
		return RowAdd
	case '-':
		return RowDel
	default:
		return RowContext
	}
}

// splitLines splits on '\n' and strips a trailing '\r' per line, mirroring
// meat's splitSourceLines: a single trailing newline produces no empty final
// line, so the row model does not gain a spurious blank row.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			out = append(out, strings.TrimSuffix(text, "\r"))
			break
		}
		out = append(out, strings.TrimSuffix(text[:i], "\r"))
		text = text[i+1:]
	}
	return out
}

// The following helpers are faithful ports of meat/diff.go's structural
// detectors, kept here so diffview stays independent of meat's unexported
// internals while classifying exactly as meat does.

func isGitDiffHeader(line string) bool {
	return strings.HasPrefix(line, "diff --git ")
}

// isRawOldFileHeader reports whether lines[i]/lines[i+1] are a "--- x"/"+++ y"
// file-marker pair. Requiring the pair is what lets a lone "-- x" or "++ y"
// inside a hunk be read as removed/added source rather than a file header.
func isRawOldFileHeader(lines []string, index int) bool {
	return index+1 < len(lines) &&
		isFileMarker(lines[index], "---") &&
		isFileMarker(lines[index+1], "+++")
}

func isFileMarker(line, marker string) bool {
	return strings.HasPrefix(line, marker+" ") || strings.HasPrefix(line, marker+"\t")
}

func isNoNewlineMarker(line string) bool {
	return line == `\ No newline at end of file`
}

// isFixedFoldLine reports whether a hunk line is a machine-generated fold: a
// marker byte followed by whitespace and exactly "...". Ported from meat.
func isFixedFoldLine(line string) bool {
	if len(line) < 2 || (line[0] != '+' && line[0] != '-' && line[0] != ' ') {
		return false
	}
	return strings.TrimSpace(line[1:]) == "..."
}
