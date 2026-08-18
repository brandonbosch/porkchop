package diffview

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// fileDigests hashes each file's own section of a diff, giving every file in the
// change a stable content identity.
//
// This is what a "viewed" marker is keyed to. Keying it to the cache key instead
// — the hash of the whole diff, which is what names a cached result — would be a
// line of code, but it would reset every marker the moment any file in the change
// moved. That is the wrong behaviour for the case the marker exists for: reviewing
// `main...HEAD` while an agent keeps committing, where the whole diff changes
// constantly and almost none of the individual files do. Per-file digests make
// "viewed" mean "I have read this file's change as it currently stands", so a file
// the new commit did not touch stays checked off and a file it did touch quietly
// un-checks itself.
//
// Digests are taken from the *original* diff, never the reading diff. A hash of
// the abridgement would also move when the model or the rubric changed, silently
// un-viewing files for a reason that has nothing to do with the files.
//
// The section is hashed verbatim, `index` lines and all. That means a rebase which
// leaves a file's hunks textually identical still un-views it, because the blob
// ids in its header moved. This is deliberate: erring toward un-viewing costs a
// reviewer a second look, while erring the other way silently marks unread content
// as read — the same "degrade toward showing more" posture Align takes.
func fileDigests(rawRows []Row, files []string) map[string]string {
	if len(rawRows) == 0 {
		return nil
	}
	// One accumulating hash per path rather than one pass per file, so a path that
	// somehow appears in two places contributes both, in order.
	acc := make(map[string]hash.Hash)
	for i, r := range rawRows {
		path := files[i]
		// Lines ahead of the first file header (a `git show` commit message, say)
		// belong to no file and are part of no file's identity.
		if path == "" {
			continue
		}
		h, ok := acc[path]
		if !ok {
			h = sha256.New()
			// Bind the path into its own digest so two files with byte-identical
			// sections — or a rename between them — cannot share a marker.
			h.Write([]byte(path))
			h.Write([]byte{0})
			acc[path] = h
		}
		h.Write([]byte(r.Text))
		h.Write([]byte{'\n'})
	}
	if len(acc) == 0 {
		return nil
	}
	out := make(map[string]string, len(acc))
	for path, h := range acc {
		out[path] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

// FileDigests is fileDigests for a caller holding only the diff text. Align fills
// Alignment.Digests from the parse it already has; this is for anything that wants
// the identities without an alignment.
func FileDigests(raw string) map[string]string {
	rawRows := Parse(raw)
	return fileDigests(rawRows, rawFileNames(rawRows))
}
