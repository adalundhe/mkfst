package vfs

import (
	"bytes"
	"compress/flate"
	"io"
	"sort"
	"time"
)

// nowNanos returns time.Now().UTC().UnixNano(). Wrapped so tests can stub
// without reaching for the time package's monotonic peculiarities.
var nowNanos = func() int64 { return time.Now().UTC().UnixNano() }

// compressColdChunksLocked compresses uncompressed chunks coldest-first
// (lowest accessAt) until at least targetFreed bytes have been returned to
// the budget, or until no compressible chunks remain. Returns the actual
// number of bytes freed.
//
// Caller must hold s.mu (write lock). Compression itself happens under the
// lock — DEFLATE level 1 runs at ~500 MB/s on commodity CPUs, so even a
// 100 MB pass adds ~200 ms of held-lock latency. That's the price of strict
// no-spill semantics; the alternative (drop the lock to compress, risk a
// concurrent Release race) is not worth the bookkeeping.
func (s *ChunkStore) compressColdChunksLocked(targetFreed int64) int64 {
	if targetFreed <= 0 {
		return 0
	}
	candidates := s.coldCandidatesLocked()
	var freed int64
	for _, hash := range candidates {
		if freed >= targetFreed {
			break
		}
		entry := s.chunks[hash]
		if entry == nil || entry.compressed {
			continue
		}
		raw := entry.content
		if len(raw) == 0 {
			continue
		}
		compressed, err := flateCompress(raw)
		if err != nil || len(compressed) >= len(raw) {
			// Either DEFLATE failed or the content is already dense (random,
			// already-compressed, or simply too small to win). Skip.
			continue
		}
		delta := int64(len(raw) - len(compressed))
		entry.content = compressed
		entry.compressed = true
		// rawSize stays at the original len(raw) — set at Put time.
		s.bytes.Add(-delta)
		freed += delta
	}
	if freed > 0 {
		s.compressionPasses.Add(1)
	}
	return freed
}

// coldCandidatesLocked returns chunk hashes sorted by ascending accessAt
// (oldest first). Stable secondary key on Hash bytes keeps the order
// deterministic across compression passes that share access timestamps —
// useful for tests and for reproducing pass behavior across runs.
func (s *ChunkStore) coldCandidatesLocked() []Hash {
	hashes := make([]Hash, 0, len(s.chunks))
	for h := range s.chunks {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		ai := s.chunks[hashes[i]].accessAt.Load()
		aj := s.chunks[hashes[j]].accessAt.Load()
		if ai != aj {
			return ai < aj
		}
		// Lex compare hash bytes for tie-break determinism.
		return bytes.Compare(hashes[i][:], hashes[j][:]) < 0
	})
	return hashes
}

// flateCompress encodes content with DEFLATE level 1 (BestSpeed). Level 1
// hits ~2-3x compression on typical text and binaries (source files,
// configs, manifests) at ~500 MB/s on commodity CPUs — fast enough that
// compression latency does not dominate write throughput.
func flateCompress(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(content) / 2)
	writer, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(content); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flateDecompress inverts flateCompress. Used by Get when serving a
// compressed chunk.
func flateDecompress(compressed []byte) ([]byte, error) {
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer reader.Close()
	return io.ReadAll(reader)
}
