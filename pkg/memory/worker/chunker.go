package worker

import (
	"strings"
	"unicode/utf8"

	"github.com/smol-platform/smol-agents/pkg/memory"
)

// ChunkSpec controls how a document is split into chunks before embedding.
// Mirrors the ChunkSpec field in MemoryRetrieverSpec (CRD layer) but is kept
// as a plain struct here to avoid importing the operator API types.
type ChunkSpec struct {
	// MaxBytes is the maximum byte size per chunk. 0 means no chunking:
	// the document is treated as a single chunk.
	MaxBytes int

	// OverlapBytes is how many bytes of the previous chunk are repeated at
	// the start of the next (sliding window). Must be < MaxBytes.
	OverlapBytes int
}

// Chunk splits doc.Content into overlapping text chunks according to spec.
// Each returned Chunk has its Text, StartByte, EndByte, and Index set;
// DocumentID is set by the caller (worker) after the document is stored.
//
// The split is rune-aligned: we never split inside a multi-byte UTF-8 sequence.
// When MaxBytes == 0, a single chunk covering the whole document is returned.
func Chunk(doc memory.Document, spec ChunkSpec) []memory.Chunk {
	content := string(doc.Content)
	if spec.MaxBytes <= 0 || len(content) <= spec.MaxBytes {
		return []memory.Chunk{{
			Index:     0,
			Text:      content,
			StartByte: 0,
			EndByte:   len(doc.Content),
		}}
	}

	overlap := spec.OverlapBytes
	if overlap < 0 || overlap >= spec.MaxBytes {
		overlap = 0
	}

	var chunks []memory.Chunk
	start := 0
	for start < len(content) {
		end := start + spec.MaxBytes
		if end > len(content) {
			end = len(content)
		}
		// Align end to rune boundary: back up until we land on a rune start.
		for end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
		text := content[start:end]

		// Prefer to break at a word boundary (space, newline) when we're not
		// at the document end, to avoid cutting mid-word.
		if end < len(content) {
			if idx := strings.LastIndexAny(text, " \n\t"); idx > 0 {
				end = start + idx + 1 // include the whitespace
				text = content[start:end]
			}
		}

		chunks = append(chunks, memory.Chunk{
			Index:     len(chunks),
			Text:      text,
			StartByte: start,
			EndByte:   end,
		})

		next := end - overlap
		if next <= start {
			// Safety: always advance to avoid infinite loop.
			next = start + 1
		}
		start = next
	}
	return chunks
}
