package quickjs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

// === WASM memory marshaling ===
//
// QuickJS lives inside the WASM linear memory. To pass strings or
// buffers across the boundary we malloc a region inside WASM, write
// our bytes, hand the pointer to QuickJS, then free.
//
// Strings/buffers returning FROM WASM use the helper layer's "packed
// pointer" convention: the C export returns an i32 pointing at an
// 8-byte struct laid out as (uint32 addr, uint32 size) — high 32
// bits are the data address, low 32 bits the byte length.

// malloc returns a fresh WASM-side buffer of `size` bytes. Caller
// must call free() with the returned pointer.
func (r *Runtime) malloc(ctx context.Context, size int) (uint32, error) {
	if size < 0 {
		return 0, errors.New("malloc: negative size")
	}
	res, err := r.mallocFn.Call(ctx, uint64(size))
	if err != nil {
		return 0, fmt.Errorf("malloc(%d): %w", size, err)
	}
	ptr := uint32(res[0])
	if ptr == 0 && size > 0 {
		return 0, fmt.Errorf("malloc(%d) returned null (likely OOM)", size)
	}
	return ptr, nil
}

// free releases a WASM-side buffer.
func (r *Runtime) free(ctx context.Context, ptr uint32) {
	if ptr == 0 {
		return
	}
	_, _ = r.freeFn.Call(ctx, uint64(ptr))
}

// writeCString allocates a NUL-terminated copy of s in WASM memory
// and returns the pointer. Caller must free.
func (r *Runtime) writeCString(ctx context.Context, s string) (uint32, error) {
	ptr, err := r.malloc(ctx, len(s)+1)
	if err != nil {
		return 0, err
	}
	if !r.memory.Write(ptr, []byte(s)) {
		r.free(ctx, ptr)
		return 0, errors.New("writeCString: out-of-bounds write")
	}
	if !r.memory.WriteByte(ptr+uint32(len(s)), 0) {
		r.free(ctx, ptr)
		return 0, errors.New("writeCString: out-of-bounds NUL write")
	}
	return ptr, nil
}

// writeBytes allocates and writes a copy of b in WASM memory.
// Caller must free.
func (r *Runtime) writeBytes(ctx context.Context, b []byte) (uint32, error) {
	if len(b) == 0 {
		// malloc(0) is implementation-defined; return a safe non-zero
		// dummy that free() tolerates.
		return r.malloc(ctx, 1)
	}
	ptr, err := r.malloc(ctx, len(b))
	if err != nil {
		return 0, err
	}
	if !r.memory.Write(ptr, b) {
		r.free(ctx, ptr)
		return 0, errors.New("writeBytes: out-of-bounds write")
	}
	return ptr, nil
}

// readCString reads a NUL-terminated string from WASM memory at
// ptr. Bounded scan up to maxLen to defend against missing NUL.
func (r *Runtime) readCString(ptr uint32, maxLen int) (string, error) {
	if maxLen <= 0 {
		maxLen = 1 << 20 // 1 MiB safety cap
	}
	buf, ok := r.memory.Read(ptr, uint32(maxLen))
	if !ok {
		return "", errors.New("readCString: out-of-bounds")
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return "", errors.New("readCString: missing NUL within bound")
}

// readBytes reads exactly n bytes from WASM memory at ptr.
func (r *Runtime) readBytes(ptr uint32, n uint32) ([]byte, error) {
	buf, ok := r.memory.Read(ptr, n)
	if !ok {
		return nil, errors.New("readBytes: out-of-bounds")
	}
	out := make([]byte, n)
	copy(out, buf)
	return out, nil
}

// unpackPtr decodes the helper layer's "packed pointer": a WASM
// pointer to an 8-byte struct (uint32 addr, uint32 size) where addr
// is the high 32 bits and size is the low 32 bits — both little-
// endian. The returned data is a copy; the packed-pointer block is
// freed before return.
func (r *Runtime) unpackPtr(ctx context.Context, packedPtr uint32) ([]byte, error) {
	if packedPtr == 0 {
		return nil, nil
	}
	hdr, ok := r.memory.Read(packedPtr, 8)
	if !ok {
		return nil, errors.New("unpackPtr: header oob")
	}
	// Layout: bytes [0..4]=size LE, bytes [4..8]=addr LE  (low/high
	// of the conceptual uint64 packed value).
	size := binary.LittleEndian.Uint32(hdr[0:4])
	addr := binary.LittleEndian.Uint32(hdr[4:8])
	defer r.free(ctx, packedPtr)
	if size == 0 || addr == 0 {
		return nil, nil
	}
	data, ok := r.memory.Read(addr, size)
	if !ok {
		return nil, errors.New("unpackPtr: data oob")
	}
	out := make([]byte, size)
	copy(out, data)
	r.free(ctx, addr)
	return out, nil
}

// readU64 reads a little-endian uint64 from WASM memory.
func (r *Runtime) readU64(ptr uint32) (uint64, error) {
	buf, ok := r.memory.Read(ptr, 8)
	if !ok {
		return 0, errors.New("readU64: oob")
	}
	return binary.LittleEndian.Uint64(buf), nil
}

// writeU64 writes a little-endian uint64 to WASM memory.
func (r *Runtime) writeU64(ptr uint32, v uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	if !r.memory.Write(ptr, buf[:]) {
		return errors.New("writeU64: oob")
	}
	return nil
}
