package desync

import (
	"crypto/sha512"
	"fmt"
	"io"
	"os"
)

// FileSeed is used to copy or clone blocks from an existing index+blob during
// file extraction.
type FileSeed struct {
	srcFile    string
	index      Index
	pos        map[ChunkID][]int
	canReflink bool
}

// NewIndexSeed initializes a new seed that uses an existing index and its blob
func NewIndexSeed(dstFile string, srcFile string, index Index) (*FileSeed, error) {
	s := FileSeed{
		srcFile:    srcFile,
		pos:        make(map[ChunkID][]int),
		index:      index,
		canReflink: CanClone(dstFile, srcFile),
	}
	for i, c := range s.index.Chunks {
		s.pos[c.ID] = append(s.pos[c.ID], i)
	}
	return &s, nil
}

// LongestMatchWith returns the longest sequence of of chunks anywhere in Source
// that match b starting at b[0]. If there is no match, it returns nil
func (s *FileSeed) LongestMatchWith(chunks []IndexChunk) (int, SeedSegment) {
	if len(chunks) == 0 || len(s.index.Chunks) == 0 {
		return 0, nil
	}
	pos, ok := s.pos[chunks[0].ID]
	if !ok {
		return 0, nil
	}
	// From every position of b[0] in the source, find a slice of
	// matching chunks. Then return the longest of those slices.
	var (
		match []IndexChunk
		max   int
	)
	for _, p := range pos {
		m := s.maxMatchFrom(chunks, p)
		if len(m) > max {
			match = m
			max = len(m)
		}
	}
	return max, newFileSeedSegment(s.srcFile, match, s.canReflink, true)
}

// Returns a slice of chunks from the seed. Compares chunks from position 0
// with seed chunks starting at p.
func (s *FileSeed) maxMatchFrom(chunks []IndexChunk, p int) []IndexChunk {
	if len(chunks) == 0 {
		return nil
	}
	var (
		sp int
		dp = p
	)
	for {
		if dp >= len(s.index.Chunks) || sp >= len(chunks) {
			break
		}
		if chunks[sp].ID != s.index.Chunks[dp].ID {
			break
		}
		dp++
		sp++
	}
	return s.index.Chunks[p:dp]
}

type fileSeedSegment struct {
	file           string
	chunks         []IndexChunk
	canReflink     bool
	needValidation bool
}

func newFileSeedSegment(file string, chunks []IndexChunk, canReflink, needValidation bool) *fileSeedSegment {
	return &fileSeedSegment{
		canReflink:     canReflink,
		needValidation: needValidation,
		file:           file,
		chunks:         chunks,
	}
}

func (f *fileSeedSegment) Size() uint64 {
	if len(f.chunks) == 0 {
		return 0
	}
	last := f.chunks[len(f.chunks)-1]
	return last.Start + last.Size - f.chunks[0].Start
}

func (s *fileSeedSegment) WriteInto(dst *os.File, offset, length, blocksize uint64) error {
	if length != s.Size() {
		return fmt.Errorf("unable to copy %d bytes from %s to %s : wrong size", length, s.file, dst.Name())
	}
	src, err := os.Open(s.file)
	if err != nil {
		return err
	}
	defer src.Close()

	// Make sure the data we're planning on pulling from the file matches what
	// the index says it is if that's required.
	if s.needValidation {
		if err := s.validate(src); err != nil {
			return err
		}
	}

	// Do a straight copy if reflinks are not supported
	if !s.canReflink {
		return s.copy(dst, src, s.chunks[0].Start, length, offset)
	}
	return s.clone(dst, src, s.chunks[0].Start, length, offset, blocksize)
}

// Compares all chunks in this slice of the seed index to the underlying data
// and fails if they don't match.
func (s *fileSeedSegment) validate(src *os.File) error {
	for _, c := range s.chunks {
		b := make([]byte, c.Size)
		if _, err := src.ReadAt(b, int64(c.Start)); err != nil {
			return err
		}
		sum := sha512.Sum512_256(b)
		if sum != c.ID {
			return fmt.Errorf("seed index for %s doesn't match its data", s.file)
		}
	}
	return nil
}

// Performs a plain copy of everything in the seed to the target, not cloning
// of blocks.
func (s *fileSeedSegment) copy(dst, src *os.File, srcOffset, srcLength, dstOffset uint64) error {
	if _, err := dst.Seek(int64(dstOffset), os.SEEK_SET); err != nil {
		return err
	}
	if _, err := src.Seek(int64(srcOffset), os.SEEK_SET); err != nil {
		return err
	}
	_, err := io.CopyN(dst, src, int64(srcLength))
	return err
}

// Reflink the overlapping blocks in the two ranges and copy the bit before and
// after the blocks.
func (s *fileSeedSegment) clone(dst, src *os.File, srcOffset, srcLength, dstOffset, blocksize uint64) error {
	if srcOffset%blocksize != dstOffset%blocksize {
		return fmt.Errorf("reflink ranges not aligned between %s and %s", src.Name(), dst.Name())
	}

	srcAlignStart := (srcOffset/blocksize + 1) * blocksize
	srcAlignEnd := (srcOffset + srcLength) / blocksize * blocksize
	dstAlignStart := (dstOffset/blocksize + 1) * blocksize
	alignLength := srcAlignEnd - srcAlignStart
	dstAlignEnd := dstAlignStart + alignLength

	// fill the area before the first aligned block
	if err := s.copy(dst, src, srcOffset, srcAlignStart-srcOffset, dstOffset); err != nil {
		return err
	}
	// fill the area after the last aligned block
	if err := s.copy(dst, src, srcAlignEnd, srcOffset+srcLength-srcAlignEnd, dstAlignEnd); err != nil {
		return err
	}
	// close the aligned blocks
	return CloneRange(dst, src, srcAlignStart, alignLength, dstAlignStart)
}
