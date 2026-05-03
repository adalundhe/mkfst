package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

// HashSize is the byte length of a content-addressable chunk hash.
const HashSize = 32

var (
	// ErrChunkNotFound is returned when a Get/Acquire/Release targets a hash
	// that's not in the store.
	ErrChunkNotFound = errors.New("vfs: chunk not found")
	// ErrMemoryPressure is returned by Put when accepting the chunk would
	// push the store past its configured memoryLimit.
	ErrMemoryPressure = errors.New("vfs: memory pressure")
	// ErrInvalidRange is returned by extent ops when start/end bounds are
	// negative, inverted, or past the file size.
	ErrInvalidRange = errors.New("vfs: invalid range")
	// ErrNegativeOffset is returned when a read/write offset is < 0.
	ErrNegativeOffset = errors.New("vfs: negative offset")
	// ErrMissingRealRead is returned when an extent points to a host path but
	// no RealFileReader was supplied to satisfy the read.
	ErrMissingRealRead = errors.New("vfs: missing real file reader")
	// ErrExtentCorruption is returned when the extent list of a file body
	// becomes non-contiguous, overlapping, or otherwise inconsistent.
	ErrExtentCorruption = errors.New("vfs: extent layout is corrupt")
)

// Hash is a SHA-256 digest used as the content-addressable key for blobs in
// ChunkStore.
type Hash [HashSize]byte

// SumHash returns the SHA-256 of content as a Hash.
func SumHash(content []byte) Hash {
	return Hash(sha256.Sum256(content))
}

// String returns the lowercase hex form of the hash.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// Short returns the first 4 bytes (8 hex chars) — useful in logs.
func (h Hash) Short() string {
	return hex.EncodeToString(h[:4])
}

// IsZero reports whether the hash is the all-zero sentinel.
func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// ChunkStoreStats is a snapshot of the chunk store's footprint and lifetime
// counters. Returned by ChunkStore.Stats; intended for telemetry and tests.
type ChunkStoreStats struct {
	Chunks            int   // distinct hashes currently retained
	Bytes             int64 // current physical bytes (compressed for DEFLATE'd chunks)
	RawBytes          int64 // logical (uncompressed) bytes — what callers see
	MemoryLimit       int64 // configured ceiling on Bytes; 0 means unbounded
	Refs              int64 // sum of all per-chunk refcounts
	UniquePutCount    int64 // lifetime new-content insertions
	DedupHitCount     int64 // lifetime puts that matched an existing hash
	RejectedPutCount  int64 // lifetime puts denied even after a compression pass
	CompressedChunks  int   // chunks currently held in compressed form
	BytesSaved        int64 // current Bytes saved by compression (RawBytes - Bytes)
	CompressionPasses int64 // lifetime full compress-cold-chunks passes
}

// chunkEntry is the per-chunk record. content holds either the raw or the
// DEFLATE-compressed payload; compressed/rawSize discriminate. accessAt is a
// unix-nano timestamp updated atomically on every Get so compression-pass
// candidate ordering reflects real usage without requiring the store lock.
type chunkEntry struct {
	content    []byte
	refs       int32
	compressed bool
	rawSize    int          // original size when compressed; equals len(content) otherwise
	accessAt   atomic.Int64 // unix nanos
}

// ChunkStore is a process-local, content-addressable byte store with
// reference counting, an optional memory ceiling, and on-demand DEFLATE
// compression of cold chunks when the ceiling is reached.
//
// Files held by VFS are decomposed into one or more extents; each blob extent
// references a chunk by Hash. Identical content stored under different paths
// shares a single chunk. When the last reference to a chunk drops, the chunk
// is freed.
//
// Compression model: when Put would push physical bytes past memoryLimit,
// the store walks chunks coldest-first and DEFLATEs each uncompressed entry
// until enough bytes are freed (or no more candidates remain). If the post-
// compression footprint still exceeds the limit, Put returns ErrMemoryPressure.
// Reads against a compressed chunk decompress on the fly and try to inflate
// in place (re-reserving the delta against the budget). When inflation
// can't fit, the read is served from a transient buffer and the chunk stays
// compressed in storage — correctness preserved, no silent disk spill.
//
// Concurrency: a single RWMutex guards map shape and entry mutations.
// accessAt updates use the entry's atomic field outside the lock so reads
// don't serialize.
type ChunkStore struct {
	mu                sync.RWMutex
	chunks            map[Hash]*chunkEntry
	memoryLimit       int64
	bytes             atomic.Int64 // current physical (compressed-aware) bytes
	rawBytes          atomic.Int64 // current logical bytes
	uniquePuts        atomic.Int64
	dedupHits         atomic.Int64
	rejected          atomic.Int64
	compressionPasses atomic.Int64
}

// NewChunkStore returns an empty store with the given byte ceiling. A
// memoryLimit of 0 (or negative) disables both the ceiling and the
// compression machinery — Put never returns ErrMemoryPressure and chunks
// always stay uncompressed.
func NewChunkStore(memoryLimit int64) *ChunkStore {
	return &ChunkStore{
		chunks:      make(map[Hash]*chunkEntry),
		memoryLimit: memoryLimit,
	}
}

// Put stores content (or increments the refcount of an existing match) and
// returns its hash. The boolean is true when content matched an existing
// chunk (dedup hit). When unbounded, never returns ErrMemoryPressure. When
// bounded, attempts a cold-chunk compression pass before rejecting.
func (s *ChunkStore) Put(content []byte) (Hash, bool, error) {
	hash := SumHash(content)

	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, ok := s.chunks[hash]; ok {
		entry.refs++
		entry.accessAt.Store(nowNanos())
		s.dedupHits.Add(1)
		return hash, true, nil
	}

	need := int64(len(content))
	if s.memoryLimit > 0 {
		if s.bytes.Load()+need > s.memoryLimit {
			freed := s.compressColdChunksLocked(s.bytes.Load() + need - s.memoryLimit)
			_ = freed
			if s.bytes.Load()+need > s.memoryLimit {
				s.rejected.Add(1)
				return Hash{}, false, ErrMemoryPressure
			}
		}
	}

	cloned := make([]byte, len(content))
	copy(cloned, content)
	entry := &chunkEntry{
		content: cloned,
		refs:    1,
		rawSize: len(content),
	}
	entry.accessAt.Store(nowNanos())
	s.chunks[hash] = entry
	s.bytes.Add(need)
	s.rawBytes.Add(need)
	s.uniquePuts.Add(1)
	return hash, false, nil
}

// Acquire bumps the refcount for an existing chunk. Returns ErrChunkNotFound
// if the hash isn't stored.
func (s *ChunkStore) Acquire(hash Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.chunks[hash]
	if !ok {
		return ErrChunkNotFound
	}
	entry.refs++
	return nil
}

// Release decrements the refcount and frees the chunk's memory when the
// count reaches zero. Returns ErrChunkNotFound if the hash isn't stored.
func (s *ChunkStore) Release(hash Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.chunks[hash]
	if !ok {
		return ErrChunkNotFound
	}
	if entry.refs <= 1 {
		s.bytes.Add(-int64(len(entry.content)))
		s.rawBytes.Add(-int64(entry.rawSize))
		delete(s.chunks, hash)
		return nil
	}
	entry.refs--
	return nil
}

// Get returns a copy of the chunk's logical (uncompressed) bytes. Returns
// ErrChunkNotFound if the hash isn't stored. Compressed chunks are inflated
// on the fly; the store also tries to re-replace the in-store payload with
// the inflated version (if memory permits) so subsequent Gets are cheap.
func (s *ChunkStore) Get(hash Hash) ([]byte, error) {
	s.mu.RLock()
	entry, ok := s.chunks[hash]
	if !ok {
		s.mu.RUnlock()
		return nil, ErrChunkNotFound
	}
	if !entry.compressed {
		out := make([]byte, len(entry.content))
		copy(out, entry.content)
		entry.accessAt.Store(nowNanos())
		s.mu.RUnlock()
		return out, nil
	}
	// Compressed path. Snapshot what we need, drop the read lock, decompress
	// outside any lock (DEFLATE is the expensive part), then optionally
	// upgrade in place under a write lock.
	compressedCopy := make([]byte, len(entry.content))
	copy(compressedCopy, entry.content)
	rawSize := entry.rawSize
	entry.accessAt.Store(nowNanos())
	s.mu.RUnlock()

	raw, err := flateDecompress(compressedCopy)
	if err != nil {
		return nil, err
	}
	if len(raw) != rawSize {
		return nil, ErrExtentCorruption
	}

	// Best-effort in-place inflate: requires a write lock and budget room.
	// On failure we still return the raw bytes — correctness > efficiency.
	s.mu.Lock()
	if entry, ok := s.chunks[hash]; ok && entry.compressed {
		delta := int64(rawSize - len(entry.content))
		if s.memoryLimit <= 0 || s.bytes.Load()+delta <= s.memoryLimit {
			entry.content = make([]byte, rawSize)
			copy(entry.content, raw)
			entry.compressed = false
			s.bytes.Add(delta)
		}
	}
	s.mu.Unlock()

	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

// Has reports whether hash is currently stored.
func (s *ChunkStore) Has(hash Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.chunks[hash]
	return ok
}

// RefCount returns the current reference count for hash, or 0 if absent.
func (s *ChunkStore) RefCount(hash Hash) int32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry, ok := s.chunks[hash]; ok {
		return entry.refs
	}
	return 0
}

// Stats returns a snapshot of the store's current footprint and lifetime
// counters.
func (s *ChunkStore) Stats() ChunkStoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var refs int64
	var compressed int
	for _, entry := range s.chunks {
		refs += int64(entry.refs)
		if entry.compressed {
			compressed++
		}
	}
	bytes := s.bytes.Load()
	rawBytes := s.rawBytes.Load()

	return ChunkStoreStats{
		Chunks:            len(s.chunks),
		Bytes:             bytes,
		RawBytes:          rawBytes,
		MemoryLimit:       s.memoryLimit,
		Refs:              refs,
		UniquePutCount:    s.uniquePuts.Load(),
		DedupHitCount:     s.dedupHits.Load(),
		RejectedPutCount:  s.rejected.Load(),
		CompressedChunks:  compressed,
		BytesSaved:        rawBytes - bytes,
		CompressionPasses: s.compressionPasses.Load(),
	}
}

// ExtentKind discriminates the three sources of bytes a file body can pull
// from: a stored chunk, a host file, or an implicit zero region (sparse).
type ExtentKind uint8

const (
	// ExtentBlob: bytes come from a chunk in the ChunkStore.
	ExtentBlob ExtentKind = iota
	// ExtentRealFile: bytes come from a host file at RealPath, RealOffset.
	// Used by the host overlay so passthrough reads don't copy content into
	// memory.
	ExtentRealFile
	// ExtentZero: an implicit zero region (sparse hole). Holds no storage.
	ExtentZero
)

// Extent describes a contiguous logical range of a file body and where its
// bytes come from. Extents within a body are kept sorted by LogicalStart and
// are non-overlapping; together they cover [0, body.Size).
type Extent struct {
	LogicalStart int64      // inclusive start of the range in file coords
	Length       int64      // length in bytes; must be > 0
	Kind         ExtentKind // source of the bytes
	BlobHash     Hash       // ExtentBlob: chunk hash
	BlobOffset   int64      // ExtentBlob: byte offset within the chunk
	RealPath     string     // ExtentRealFile: host path
	RealOffset   int64      // ExtentRealFile: byte offset within the host file
}

// End returns LogicalStart + Length (exclusive end of the range).
func (e Extent) End() int64 {
	return e.LogicalStart + e.Length
}

func (e Extent) shift(delta int64) Extent {
	e.LogicalStart += delta
	return e
}

func (e Extent) subrange(start, end int64) Extent {
	out := e
	delta := start - e.LogicalStart
	out.LogicalStart = start
	out.Length = end - start
	switch e.Kind {
	case ExtentBlob:
		out.BlobOffset += delta
	case ExtentRealFile:
		out.RealOffset += delta
	}
	return out
}

// RealFileReader abstracts host-file reads for ExtentRealFile extents. The
// VFS stays decoupled from the OS file API so tests can substitute a fake.
type RealFileReader interface {
	ReadAt(path string, p []byte, off int64) (int, error)
}

// OSRealFileReader is the production RealFileReader; opens path each call
// and delegates to *os.File.ReadAt. Suitable for cold reads. The host
// overlay layers an LRU cache in front of it for repeated reads.
type OSRealFileReader struct{}

// ReadAt opens path, reads from off, and closes. Errors propagate
// transparently.
func (OSRealFileReader) ReadAt(path string, p []byte, off int64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.ReadAt(p, off)
}

// FileBody is the byte payload of a file inode. It's an extent list rather
// than a single buffer so partial overwrites, sparse files, and host-file
// passthrough don't require copying or re-allocating the whole content.
type FileBody struct {
	size    int64
	extents []Extent
}

// NewEmptyFileBody returns a body of length 0.
func NewEmptyFileBody() *FileBody {
	return &FileBody{}
}

// NewBlobFileBody returns a body whose entire content comes from a single
// blob chunk.
func NewBlobFileBody(hash Hash, size int64) *FileBody {
	if size <= 0 {
		return NewEmptyFileBody()
	}
	return &FileBody{
		size: size,
		extents: []Extent{{
			LogicalStart: 0,
			Length:       size,
			Kind:         ExtentBlob,
			BlobHash:     hash,
		}},
	}
}

// NewChunkedFileBody splits content into chunkSize pieces, stores each in
// store, and returns a body whose extents reference them in order. A
// chunkSize of <= 0 defaults to 64 KiB.
func NewChunkedFileBody(content []byte, store *ChunkStore, chunkSize int) (*FileBody, error) {
	if len(content) == 0 {
		return NewEmptyFileBody(), nil
	}
	if store == nil {
		return nil, ErrChunkNotFound
	}
	if chunkSize <= 0 {
		chunkSize = 64 << 10
	}
	extents := make([]Extent, 0, (len(content)+chunkSize-1)/chunkSize)
	for offset := 0; offset < len(content); offset += chunkSize {
		end := offset + chunkSize
		if end > len(content) {
			end = len(content)
		}
		hash, _, err := store.Put(content[offset:end])
		if err != nil {
			return nil, err
		}
		extents = append(extents, Extent{
			LogicalStart: int64(offset),
			Length:       int64(end - offset),
			Kind:         ExtentBlob,
			BlobHash:     hash,
		})
	}
	return &FileBody{
		size:    int64(len(content)),
		extents: extents,
	}, nil
}

// NewRealFileBody returns a body whose entire content is a passthrough
// reference to a host file at realPath. Used by the host overlay; reads
// resolve via RealFileReader without copying into memory.
func NewRealFileBody(realPath string, size int64) *FileBody {
	if size <= 0 {
		return NewEmptyFileBody()
	}
	return &FileBody{
		size: size,
		extents: []Extent{{
			LogicalStart: 0,
			Length:       size,
			Kind:         ExtentRealFile,
			RealPath:     realPath,
		}},
	}
}

// Clone returns a deep copy of the body's extent list. The underlying chunks
// are not copied — they're refcounted in ChunkStore — so callers that intend
// to retain a clone independently must Acquire each blob hash.
func (b *FileBody) Clone() *FileBody {
	if b == nil {
		return NewEmptyFileBody()
	}
	out := &FileBody{
		size:    b.size,
		extents: make([]Extent, len(b.extents)),
	}
	copy(out.extents, b.extents)
	return out
}

// Size returns the total logical length of the body.
func (b *FileBody) Size() int64 {
	if b == nil {
		return 0
	}
	return b.size
}

// Extents returns a copy of the body's extent list, sorted by LogicalStart.
func (b *FileBody) Extents() []Extent {
	if b == nil {
		return nil
	}
	out := make([]Extent, len(b.extents))
	copy(out, b.extents)
	return out
}

// ReadAt fills p starting at logical offset off. Returns the number of bytes
// written and io.EOF when the range extends past Size. Resolves blob extents
// through store and real-file extents through reader.
func (b *FileBody) ReadAt(p []byte, off int64, reader RealFileReader, store *ChunkStore) (int, error) {
	if b == nil || len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if off >= b.size {
		return 0, io.EOF
	}

	limit := minInt64(int64(len(p)), b.size-off)
	if err := b.ensureCoveredRange(off, off+limit); err != nil {
		return 0, err
	}

	var written int64
	for written < limit {
		pos := off + written
		idx := b.findExtentIndex(pos)
		if idx < 0 {
			return int(written), ErrExtentCorruption
		}
		ex := b.extents[idx]
		extentOffset := pos - ex.LogicalStart
		remaining := limit - written
		chunkLen := minInt64(ex.Length-extentOffset, remaining)
		dst := p[written : written+chunkLen]

		switch ex.Kind {
		case ExtentZero:
			clear(dst)
		case ExtentBlob:
			if store == nil {
				return int(written), ErrChunkNotFound
			}
			content, err := store.Get(ex.BlobHash)
			if err != nil {
				return int(written), err
			}
			copy(dst, content[ex.BlobOffset+extentOffset:ex.BlobOffset+extentOffset+chunkLen])
		case ExtentRealFile:
			if reader == nil {
				return int(written), ErrMissingRealRead
			}
			n, err := reader.ReadAt(ex.RealPath, dst, ex.RealOffset+extentOffset)
			if int64(n) != chunkLen {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return int(written) + n, err
			}
		default:
			return int(written), ErrExtentCorruption
		}
		written += chunkLen
	}

	if limit < int64(len(p)) {
		return int(limit), io.EOF
	}
	return int(limit), nil
}

// WriteAt overlays content starting at off, growing the body if needed.
// Replaced ranges are dropped; new content is stored as a single blob.
func (b *FileBody) WriteAt(off int64, content []byte, store *ChunkStore) error {
	if b == nil {
		return ErrInvalidRange
	}
	if off < 0 {
		return ErrNegativeOffset
	}
	if len(content) == 0 {
		return nil
	}
	if off > b.size {
		b.appendZero(off - b.size)
	}
	end := off + int64(len(content))
	replacedEnd := minInt64(end, b.size)
	repl, replLen, err := blobReplacement(content, store)
	if err != nil {
		return err
	}
	return b.replaceRange(off, replacedEnd, repl, replLen)
}

// InsertAt inserts content at off, shifting the tail right. off must be in
// [0, Size]. content is stored as a single blob.
func (b *FileBody) InsertAt(off int64, content []byte, store *ChunkStore) error {
	if b == nil {
		return ErrInvalidRange
	}
	if off < 0 || off > b.size {
		return ErrInvalidRange
	}
	if len(content) == 0 {
		return nil
	}
	repl, replLen, err := blobReplacement(content, store)
	if err != nil {
		return err
	}
	return b.replaceRange(off, off, repl, replLen)
}

// DeleteRange removes [start, end), shifting the tail left.
func (b *FileBody) DeleteRange(start, end int64) error {
	if b == nil {
		return ErrInvalidRange
	}
	if start < 0 || end < start || end > b.size {
		return ErrInvalidRange
	}
	return b.replaceRange(start, end, nil, 0)
}

// Truncate sets Size to size, either trimming the tail or appending a zero
// region.
func (b *FileBody) Truncate(size int64) error {
	if b == nil || size < 0 {
		return ErrInvalidRange
	}
	switch {
	case size == b.size:
		return nil
	case size < b.size:
		return b.DeleteRange(size, b.size)
	default:
		b.appendZero(size - b.size)
		return nil
	}
}

func (b *FileBody) appendZero(length int64) {
	if length <= 0 {
		return
	}
	b.extents = appendAndNormalize(b.extents, Extent{
		LogicalStart: b.size,
		Length:       length,
		Kind:         ExtentZero,
	})
	b.size += length
}

func (b *FileBody) replaceRange(start, end int64, repl []Extent, replLen int64) error {
	if start < 0 || end < start || end > b.size {
		return ErrInvalidRange
	}

	delta := replLen - (end - start)
	result := make([]Extent, 0, len(b.extents)+len(repl)+2)

	for _, ex := range b.extents {
		switch {
		case ex.End() <= start:
			result = append(result, ex)
		case ex.LogicalStart >= end:
			result = append(result, ex.shift(delta))
		default:
			if ex.LogicalStart < start {
				result = append(result, ex.subrange(ex.LogicalStart, start))
			}
			if ex.End() > end {
				right := ex.subrange(end, ex.End()).shift(delta)
				result = append(result, right)
			}
		}
	}

	for _, ex := range repl {
		result = append(result, ex.shift(start))
	}

	sort.SliceStable(result, func(i, j int) bool {
		return result[i].LogicalStart < result[j].LogicalStart
	})

	b.size += delta
	b.extents = normalizeExtents(result)
	return b.ensureCoveredRange(0, b.size)
}

func (b *FileBody) ensureCoveredRange(start, end int64) error {
	if end < start {
		return ErrInvalidRange
	}
	if start == end {
		return nil
	}
	expected := start
	for _, ex := range b.extents {
		if ex.Length <= 0 {
			return ErrExtentCorruption
		}
		if ex.LogicalStart > expected {
			return ErrExtentCorruption
		}
		if ex.End() <= expected {
			continue
		}
		if ex.LogicalStart < expected {
			expected = minInt64(ex.End(), end)
		} else {
			expected = minInt64(ex.End(), end)
		}
		if expected >= end {
			return nil
		}
	}
	if end == 0 {
		return nil
	}
	if expected < end {
		return ErrExtentCorruption
	}
	return nil
}

func (b *FileBody) findExtentIndex(pos int64) int {
	idx := sort.Search(len(b.extents), func(i int) bool {
		return b.extents[i].End() > pos
	})
	if idx >= len(b.extents) {
		return -1
	}
	ex := b.extents[idx]
	if pos < ex.LogicalStart || pos >= ex.End() {
		return -1
	}
	return idx
}

func blobReplacement(content []byte, store *ChunkStore) ([]Extent, int64, error) {
	if len(content) == 0 {
		return nil, 0, nil
	}
	if store == nil {
		return nil, 0, ErrChunkNotFound
	}
	hash, _, err := store.Put(content)
	if err != nil {
		return nil, 0, err
	}
	return []Extent{{
		LogicalStart: 0,
		Length:       int64(len(content)),
		Kind:         ExtentBlob,
		BlobHash:     hash,
	}}, int64(len(content)), nil
}

func appendAndNormalize(extents []Extent, extent Extent) []Extent {
	return normalizeExtents(append(extents, extent))
}

func normalizeExtents(extents []Extent) []Extent {
	if len(extents) == 0 {
		return nil
	}
	sort.SliceStable(extents, func(i, j int) bool {
		return extents[i].LogicalStart < extents[j].LogicalStart
	})
	out := make([]Extent, 0, len(extents))
	for _, ex := range extents {
		if ex.Length <= 0 {
			continue
		}
		if len(out) == 0 {
			out = append(out, ex)
			continue
		}
		last := out[len(out)-1]
		if canMergeExtent(last, ex) {
			last.Length += ex.Length
			out[len(out)-1] = last
			continue
		}
		out = append(out, ex)
	}
	return out
}

func canMergeExtent(a, b Extent) bool {
	if a.Kind != b.Kind || a.End() != b.LogicalStart {
		return false
	}
	switch a.Kind {
	case ExtentBlob:
		return a.BlobHash == b.BlobHash && a.BlobOffset+a.Length == b.BlobOffset
	case ExtentRealFile:
		return a.RealPath == b.RealPath && a.RealOffset+a.Length == b.RealOffset
	case ExtentZero:
		return true
	default:
		return false
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
