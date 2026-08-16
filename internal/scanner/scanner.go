// Package scanner finds uploadable videos in a directory.
package scanner

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

// Scan returns one job for every regular, top-level MP4 file in dir.
//
// Directory entries are not followed recursively. Symlinks and non-regular
// files are ignored. A positive maxBytes marks files larger than that value
// as model.JobOversize; a non-positive value means that no size limit is
// applied.
func Scan(dir string, maxBytes int64) ([]model.Job, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan %q: %w", dir, err)
	}

	files := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)

		// Lstat is intentional: Stat would follow a symlink and could make a
		// symlink to a regular file look like an eligible file.
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if !isMP4(name) {
			continue
		}

		files = append(files, fileEntry{
			name:    name,
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return compareNatural(files[i].name, files[j].name) < 0
	})

	jobs := make([]model.Job, len(files))
	for position, file := range files {
		id, randomID, err := newIDs()
		if err != nil {
			return nil, fmt.Errorf("generate job identity for %q: %w", file.path, err)
		}

		state := model.JobQueued
		if maxBytes > 0 && file.size > maxBytes {
			state = model.JobOversize
		}
		jobs[position] = model.Job{
			ID:       id,
			Position: position,
			Path:     file.path,
			Name:     file.name,
			Size:     file.size,
			ModTime:  file.modTime,
			State:    state,
			RandomID: randomID,
		}
	}

	return jobs, nil
}

// ScanDirectory is a descriptive alias for Scan.
func ScanDirectory(dir string, maxBytes int64) ([]model.Job, error) {
	return Scan(dir, maxBytes)
}

type fileEntry struct {
	name    string
	path    string
	size    int64
	modTime time.Time
}

func isMP4(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".mp4")
}

// newIDs uses crypto/rand for both identifiers. RandomID is kept positive so
// it remains convenient to use with APIs that reject negative IDs, while
// still being a non-zero int64 as required by Telegram.
func newIDs() (string, int64, error) {
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", 0, err
	}
	// Mark the random bytes as a UUIDv4 value without introducing another
	// dependency just for identifier formatting.
	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80

	// UUID formatting makes the identifier easy to inspect while the bytes
	// themselves remain cryptographically random.
	id := hex.EncodeToString(idBytes[:4]) + "-" +
		hex.EncodeToString(idBytes[4:6]) + "-" +
		hex.EncodeToString(idBytes[6:8]) + "-" +
		hex.EncodeToString(idBytes[8:10]) + "-" +
		hex.EncodeToString(idBytes[10:])

	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", 0, err
	}
	randomID := int64(binary.BigEndian.Uint64(randomBytes[:]) & ((uint64(1) << 63) - 1))
	if randomID == 0 {
		randomID = 1
	}
	return id, randomID, nil
}

// compareNatural compares filename strings by Unicode code point, treating
// ASCII decimal runs as arbitrary-precision integers. Numeric values are
// compared without converting to a machine integer, so very long runs are
// handled correctly. Equal numeric values use the shorter original run as a
// tie-breaker, followed by the original filename for deterministic ordering.
func compareNatural(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	ia, ib := 0, 0
	for ia < len(ra) && ib < len(rb) {
		if isASCIIDigit(ra[ia]) && isASCIIDigit(rb[ib]) {
			ea := ia
			for ea < len(ra) && isASCIIDigit(ra[ea]) {
				ea++
			}
			eb := ib
			for eb < len(rb) && isASCIIDigit(rb[eb]) {
				eb++
			}

			if result := compareDigitRuns(ra[ia:ea], rb[ib:eb]); result != 0 {
				return result
			}
			ia, ib = ea, eb
			continue
		}

		foldedA := unicode.ToLower(ra[ia])
		foldedB := unicode.ToLower(rb[ib])
		if foldedA < foldedB {
			return -1
		}
		if foldedA > foldedB {
			return 1
		}
		ia++
		ib++
	}

	if ia < len(ra) {
		return 1
	}
	if ib < len(rb) {
		return -1
	}
	// This also gives a deterministic ordering for distinct byte strings
	// which decode to the same rune sequence.
	return strings.Compare(a, b)
}

func compareDigitRuns(a, b []rune) int {
	aStart := firstNonZero(a)
	bStart := firstNonZero(b)
	aSignificant := a[aStart:]
	bSignificant := b[bStart:]

	// A run consisting entirely of zeroes represents one zero, not an empty
	// number. Comparing significant lengths still works if both are empty.
	if len(aSignificant) == 0 {
		aSignificant = []rune{'0'}
	}
	if len(bSignificant) == 0 {
		bSignificant = []rune{'0'}
	}

	if len(aSignificant) < len(bSignificant) {
		return -1
	}
	if len(aSignificant) > len(bSignificant) {
		return 1
	}
	for i := range aSignificant {
		if aSignificant[i] < bSignificant[i] {
			return -1
		}
		if aSignificant[i] > bSignificant[i] {
			return 1
		}
	}

	// Equal values: the shorter spelling wins (1 before 01 before 001).
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

func firstNonZero(digits []rune) int {
	for i, digit := range digits {
		if digit != '0' {
			return i
		}
	}
	return len(digits)
}

func isASCIIDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
