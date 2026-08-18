package diffview

import (
	"path"
	"strings"
)

// commentSyntax is the lexical comment forms of one language: prefixes that
// start a comment running to end of line, and delimiter pairs that open and
// close a block.
//
// This is deliberately lexical and not a parser. The question being answered is
// only "is there code for a reviewer to read behind this marker", and the answer
// is allowed to be "don't know" — an unrecognized extension, an exotic comment
// form, or a block whose opening line the reading diff kept all yield zero
// comment lines, which words the marker the ordinary way. Under-reporting costs
// a reviewer one needless expand. Over-reporting costs them a changed line they
// were told not to look at, so every judgement call below leans the first way.
type commentSyntax struct {
	line   []string
	blocks [][2]string
}

var (
	syntaxHash    = commentSyntax{line: []string{"#"}}
	syntaxSemi    = commentSyntax{line: []string{";"}}
	syntaxPercent = commentSyntax{line: []string{"%"}}
	syntaxCLike   = commentSyntax{line: []string{"//"}, blocks: [][2]string{{"/*", "*/"}}}
	syntaxCSS     = commentSyntax{blocks: [][2]string{{"/*", "*/"}}}
	syntaxMarkup  = commentSyntax{blocks: [][2]string{{"<!--", "-->"}}}
	syntaxSQL     = commentSyntax{line: []string{"--"}, blocks: [][2]string{{"/*", "*/"}}}
	syntaxLua     = commentSyntax{line: []string{"--"}, blocks: [][2]string{{"--[[", "]]"}}}
	syntaxHaskell = commentSyntax{line: []string{"--"}, blocks: [][2]string{{"{-", "-}"}}}

	// Python's triple-quoted forms are string literals, not comments, and only a
	// docstring in specific positions. They are listed because a reviewer reads
	// them as prose either way, and because the opener rule in commentScanner.next
	// keeps the data-carrying uses (a SQL or fixture heredoc, which is assigned or
	// passed and so has code before the quotes on its opening line) out.
	syntaxPython = commentSyntax{line: []string{"#"}, blocks: [][2]string{{`"""`, `"""`}, {"'''", "'''"}}}
)

// commentSyntaxByExt keys on the lowercased extension of the path the diff
// itself names. Anything absent is simply unknown, which is a supported answer.
var commentSyntaxByExt = map[string]commentSyntax{
	".py": syntaxPython, ".pyi": syntaxPython,
	".sh": syntaxHash, ".bash": syntaxHash, ".zsh": syntaxHash, ".fish": syntaxHash,
	".rb": syntaxHash, ".pl": syntaxHash, ".pm": syntaxHash, ".r": syntaxHash,
	".yaml": syntaxHash, ".yml": syntaxHash, ".toml": syntaxHash, ".nix": syntaxHash,
	".tf": syntaxHash, ".tfvars": syntaxHash, ".hcl": syntaxHash, ".cmake": syntaxHash,
	".mk": syntaxHash, ".conf": syntaxHash, ".cfg": syntaxHash, ".properties": syntaxHash,
	".ex": syntaxHash, ".exs": syntaxHash, ".jl": syntaxHash, ".env": syntaxHash,
	".dockerignore": syntaxHash, ".gitignore": syntaxHash, ".gitattributes": syntaxHash,

	".go": syntaxCLike, ".c": syntaxCLike, ".h": syntaxCLike, ".cc": syntaxCLike,
	".cpp": syntaxCLike, ".cxx": syntaxCLike, ".hpp": syntaxCLike, ".hh": syntaxCLike,
	".java": syntaxCLike, ".js": syntaxCLike, ".jsx": syntaxCLike, ".mjs": syntaxCLike,
	".cjs": syntaxCLike, ".ts": syntaxCLike, ".tsx": syntaxCLike, ".mts": syntaxCLike,
	".rs": syntaxCLike, ".swift": syntaxCLike, ".kt": syntaxCLike, ".kts": syntaxCLike,
	".scala": syntaxCLike, ".cs": syntaxCLike, ".php": syntaxCLike, ".dart": syntaxCLike,
	".zig": syntaxCLike, ".proto": syntaxCLike, ".groovy": syntaxCLike, ".gradle": syntaxCLike,
	".m": syntaxCLike, ".mm": syntaxCLike, ".glsl": syntaxCLike, ".scss": syntaxCLike,
	".less": syntaxCLike,

	".css": syntaxCSS,

	".html": syntaxMarkup, ".htm": syntaxMarkup, ".xml": syntaxMarkup, ".xsl": syntaxMarkup,
	".svg": syntaxMarkup, ".vue": syntaxMarkup, ".md": syntaxMarkup, ".markdown": syntaxMarkup,

	".sql": syntaxSQL,
	".lua": syntaxLua,
	".hs":  syntaxHaskell, ".elm": syntaxHaskell,

	".el": syntaxSemi, ".lisp": syntaxSemi, ".clj": syntaxSemi, ".cljs": syntaxSemi,
	".cljc": syntaxSemi, ".scm": syntaxSemi, ".ini": syntaxSemi,

	".erl": syntaxPercent, ".hrl": syntaxPercent, ".tex": syntaxPercent,
}

// commentSyntaxByBase covers the extensionless files common enough in a change
// to be worth naming.
var commentSyntaxByBase = map[string]commentSyntax{
	"Makefile":       syntaxHash,
	"makefile":       syntaxHash,
	"GNUmakefile":    syntaxHash,
	"Dockerfile":     syntaxHash,
	"Containerfile":  syntaxHash,
	"CMakeLists.txt": syntaxHash,
	"Jenkinsfile":    syntaxCLike,
}

// commentSyntaxFor resolves the comment forms of the file at p, which is a diff
// path and therefore always slash-separated regardless of host.
func commentSyntaxFor(p string) (commentSyntax, bool) {
	if p == "" {
		return commentSyntax{}, false
	}
	base := path.Base(p)
	if s, ok := commentSyntaxByBase[base]; ok {
		return s, true
	}
	s, ok := commentSyntaxByExt[strings.ToLower(path.Ext(base))]
	return s, ok
}

// lineClass is what one line of a hidden run holds. classBlank and classComment
// are disjoint — a line with no content is blank even inside a block comment —
// so an elision's Blank and Comment counts never double-count a line, which is
// the invariant the marker wording adds them under.
type lineClass uint8

const (
	classCode lineClass = iota
	classBlank
	classComment
)

// commentScanner walks the lines of one file, on one side of the diff, in order,
// carrying block-comment state across them.
//
// It starts outside a block and stays honest about it: state is established only
// by an opener the scanner actually saw, never assumed from context it cannot
// see. A hidden run that begins in the middle of a block therefore reads as code
// and gets the ordinary wording.
type commentScanner struct {
	syntax  commentSyntax
	closer  string
	inBlock bool
}

func (s *commentScanner) next(body string) lineClass {
	trimmed := strings.TrimSpace(body)

	if s.inBlock {
		i := strings.Index(body, s.closer)
		if i < 0 {
			if trimmed == "" {
				return classBlank
			}
			return classComment
		}
		s.inBlock = false
		if strings.TrimSpace(body[i+len(s.closer):]) != "" {
			// Code resumes after the block closes on this same line.
			return classCode
		}
		return classComment
	}

	if trimmed == "" {
		return classBlank
	}

	// Blocks are tried before line prefixes because some languages nest one
	// inside the other lexically — Lua's "--[[" opens a block but also starts
	// with its line-comment prefix "--", and matching the shorter form first
	// would swallow the opener and lose the block.
	for _, b := range s.syntax.blocks {
		open, closer := b[0], b[1]
		if !strings.HasPrefix(trimmed, open) {
			continue
		}
		rest := trimmed[len(open):]
		if i := strings.Index(rest, closer); i >= 0 {
			if strings.TrimSpace(rest[i+len(closer):]) != "" {
				return classCode
			}
			return classComment
		}
		// The block runs past this line. When the delimiters are symmetric the
		// same token both opens and closes, so a bare one is ambiguous: it is an
		// opener only if the scanner is outside a block, which is exactly what it
		// cannot know at the head of a hidden run. Requiring text after the token
		// resolves it — a Python docstring opens """Like this, while a closing
		// """ sits alone or ends a line of prose. Guessing wrong here would
		// invert the classification for the rest of the run, so it declines.
		if open == closer && rest == "" {
			return classCode
		}
		s.inBlock, s.closer = true, closer
		return classComment
	}

	for _, p := range s.syntax.line {
		if strings.HasPrefix(trimmed, p) {
			return classComment
		}
	}
	return classCode
}

// commentChanges counts how many of the changed lines in one hidden run are
// wholly comment. rows and files are parallel slices covering the run only.
//
// The two sides of the diff are scanned separately, because an added block
// comment and a removed one are different sequences of text and interleaving
// them would corrupt both. Context lines are unchanged and so belong to both
// versions: they move both scanners' block state but are never counted, since
// Changed does not count them either.
func commentChanges(rows []Row, files []string) int {
	var (
		add, del commentScanner
		file     string
		known    bool
		first    = true
		n        int
	)
	for i, r := range rows {
		f := ""
		if i < len(files) {
			f = files[i]
		}
		// A hunk header is a jump to somewhere else in the file, and a metadata
		// row is usually a jump to another file. Neither carries block state.
		if first || f != file || r.Kind == RowHunk || r.Kind == RowMeta {
			var syn commentSyntax
			syn, known = commentSyntaxFor(f)
			add, del = commentScanner{syntax: syn}, commentScanner{syntax: syn}
			file, first = f, false
		}
		if !known || len(r.Text) == 0 {
			continue
		}
		counts := isChangedLine(r)
		if !counts && r.Kind != RowContext {
			continue
		}
		body := r.Text[1:]
		switch r.Text[0] {
		case '+':
			if add.next(body) == classComment && counts {
				n++
			}
		case '-':
			if del.next(body) == classComment && counts {
				n++
			}
		default:
			add.next(body)
			del.next(body)
		}
	}
	return n
}
