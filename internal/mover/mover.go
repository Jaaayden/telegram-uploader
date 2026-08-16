// Package mover moves files into their final location without replacing an
// existing destination. An atomic no-replace rename is preferred because it
// does not require reading the file. When the source and destination are on
// different file systems, the package falls back to a verified copy.
package mover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

// ProgressFunc receives progress for one move operation.  BytesDone is
// monotonic for a single operation; MoveBatch also makes it monotonic across
// all files in the batch.  It is an alias so ordinary func(model.Progress)
// values and callers' named callback types can be passed without conversion.
type ProgressFunc = func(model.Progress)

var (
	// ErrDestinationExists is returned when the destination already exists.
	// Existing files, directories, and symlinks are all conflicts.
	ErrDestinationExists = fmt.Errorf("mover: destination already exists: %w", os.ErrExist)

	// ErrChecksumMismatch means that the source and copied destination did not
	// have the same SHA-256 digest.  The source is deliberately left in place.
	ErrChecksumMismatch = errors.New("mover: copied file checksum mismatch")
)

// moveFile is intentionally tiny.  Keeping the file operations behind this
// type lets package tests force EXDEV and inject read/sync errors without
// changing the production API.
type moveFile interface {
	io.Reader
	io.Writer
	Sync() error
	Close() error
}

type moveOps struct {
	rename        func(string, string) error
	lstat         func(string) (os.FileInfo, error)
	open          func(string) (io.ReadCloser, error)
	createPartial func(string, os.FileMode) (moveFile, error)
	remove        func(string) error
	syncDir       func(string) error
}

func defaultMoveOps() moveOps {
	return moveOps{
		rename: renameNoReplace,
		lstat:  os.Lstat,
		open: func(name string) (io.ReadCloser, error) {
			return os.Open(name)
		},
		createPartial: func(name string, mode os.FileMode) (moveFile, error) {
			return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		},
		remove:  os.Remove,
		syncDir: syncDirectory,
	}
}

// Mover performs safe file moves.  The zero value is ready for use.
//
// New is provided for callers that prefer an explicit constructor.  No state
// is shared between Mover values, which also makes independent operations safe
// to run concurrently.
type Mover struct {
	ops *moveOps
}

// newMoverWithOps is kept unexported on purpose: production callers should use
// New, while same-package tests can inject only the operation they need to
// exercise (for example a rename that returns EXDEV).
func newMoverWithOps(ops moveOps) *Mover {
	defaults := defaultMoveOps()
	if ops.rename == nil {
		ops.rename = defaults.rename
	}
	if ops.lstat == nil {
		ops.lstat = defaults.lstat
	}
	if ops.open == nil {
		ops.open = defaults.open
	}
	if ops.createPartial == nil {
		ops.createPartial = defaults.createPartial
	}
	if ops.remove == nil {
		ops.remove = defaults.remove
	}
	if ops.syncDir == nil {
		ops.syncDir = defaults.syncDir
	}
	return &Mover{ops: &ops}
}

// New returns a Mover using the operating system file operations.
func New() *Mover {
	return newMoverWithOps(defaultMoveOps())
}

// NewMover is an explicit-name alias for New.
func NewMover() *Mover { return New() }

func (m *Mover) operations() *moveOps {
	if m == nil || m.ops == nil {
		ops := defaultMoveOps()
		return &ops
	}
	return m.ops
}

func progressCallback(progress []ProgressFunc) ProgressFunc {
	if len(progress) == 0 || progress[0] == nil {
		return nil
	}
	return progress[0]
}

// Move moves source to destination.  The destination is never overwritten.
// It first attempts os.Rename; on EXDEV it copies to a hidden .partial file,
// verifies SHA-256 digests, fsyncs, atomically renames the partial file, and
// only then removes the source.
//
// The variadic callback keeps the callback optional while accepting the usual
// four-argument form: Move(ctx, source, destination, onProgress).
func Move(ctx context.Context, source, destination string, progress ...ProgressFunc) error {
	return New().Move(ctx, source, destination, progress...)
}

// MoveFile is a descriptive alias for Move.
func MoveFile(ctx context.Context, source, destination string, progress ...ProgressFunc) error {
	return Move(ctx, source, destination, progress...)
}

// Move moves source to destination using this Mover.
func (m *Mover) Move(ctx context.Context, source, destination string, progress ...ProgressFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cb := progressCallback(progress)
	ops := m.operations()

	if err := ctx.Err(); err != nil {
		return err
	}
	if source == "" || destination == "" {
		return errors.New("mover: source and destination must not be empty")
	}

	sourceInfo, err := ops.lstat(source)
	if err != nil {
		return fmt.Errorf("mover: stat source %q: %w", source, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("mover: source %q is not a regular file", source)
	}

	if err := destinationAbsent(ops, destination); err != nil {
		return err
	}

	total := sourceInfo.Size()
	if total < 0 {
		total = 0
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ops.rename(source, destination); err == nil {
		emitProgress(cb, total, total, time.Now())
		return nil
	} else if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
	} else if !isCrossDeviceError(err) {
		return fmt.Errorf("mover: rename %q to %q: %w", source, destination, err)
	}

	// A destination could have appeared while the first rename was in flight.
	// Check again before creating or installing the copied file.
	if err := destinationAbsent(ops, destination); err != nil {
		return err
	}

	return m.copyAfterEXDEV(ctx, source, destination, sourceInfo.Mode(), total, cb, ops)
}

// copyAfterEXDEV performs the durable, verified copy path.  It intentionally
// keeps the source untouched until every verification and fsync has succeeded.
func (m *Mover) copyAfterEXDEV(
	ctx context.Context,
	source, destination string,
	mode os.FileMode,
	total int64,
	cb ProgressFunc,
	ops *moveOps,
) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	sourceDigest, err := hashFile(ctx, source, ops)
	if err != nil {
		return fmt.Errorf("mover: hash source %q: %w", source, err)
	}

	partial := partialPath(destination)
	partialFile, err := ops.createPartial(partial, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("mover: partial destination %q already exists: %w", partial, ErrDestinationExists)
		}
		return fmt.Errorf("mover: create partial destination %q: %w", partial, err)
	}
	partialOwned := true
	partialClosed := false
	defer func() {
		if !partialClosed {
			_ = partialFile.Close()
		}
		if partialOwned {
			if cleanupErr := ops.remove(partial); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("mover: remove partial destination %q: %w", partial, cleanupErr))
			}
		}
	}()

	// Report a well-defined starting point, including for an empty file.
	emitProgress(cb, 0, total, time.Now())

	sourceFile, err := ops.open(source)
	if err != nil {
		return fmt.Errorf("mover: open source %q for copy: %w", source, err)
	}

	var copied int64
	copyErr := func() error {
		copied, err = copyWithContext(ctx, partialFile, sourceFile, func(done int64) {
			emitProgress(cb, done, total, time.Now())
		})
		return err
	}()
	closeSourceErr := sourceFile.Close()
	if copyErr != nil {
		return fmt.Errorf("mover: copy %q to %q: %w", source, partial, copyErr)
	}
	if closeSourceErr != nil {
		return fmt.Errorf("mover: close source %q: %w", source, closeSourceErr)
	}
	if copied != total {
		return fmt.Errorf("mover: copied %d bytes from %q, expected %d", copied, source, total)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := partialFile.Sync(); err != nil {
		return fmt.Errorf("mover: sync partial destination %q: %w", partial, err)
	}
	if err := partialFile.Close(); err != nil {
		return fmt.Errorf("mover: close partial destination %q: %w", partial, err)
	}
	partialClosed = true

	partialDigest, err := hashFile(ctx, partial, ops)
	if err != nil {
		return fmt.Errorf("mover: hash partial destination %q: %w", partial, err)
	}
	if partialDigest != sourceDigest {
		return checksumError(source, partial, sourceDigest, partialDigest)
	}

	if err := destinationAbsent(ops, destination); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ops.rename(partial, destination); errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
	} else if err != nil {
		return fmt.Errorf("mover: install destination %q: %w", destination, err)
	}
	partialOwned = false

	// If anything after this point fails, remove the destination we installed
	// and leave the source in place.  This is the same cleanup guarantee as for
	// a failed partial copy, without touching a pre-existing destination.
	destinationOwned := true
	defer func() {
		if destinationOwned && retErr != nil {
			if cleanupErr := ops.remove(destination); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("mover: remove installed destination %q: %w", destination, cleanupErr))
			}
		}
	}()

	if err := ops.syncDir(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("mover: sync destination directory %q: %w", filepath.Dir(destination), err)
	}

	destinationDigest, err := hashFile(ctx, destination, ops)
	if err != nil {
		return fmt.Errorf("mover: hash destination %q: %w", destination, err)
	}
	if destinationDigest != sourceDigest {
		return checksumError(source, destination, sourceDigest, destinationDigest)
	}

	// Re-read the source immediately before removing it.  This closes the most
	// important race in a copy move: a source changing after the initial hash
	// must never be silently deleted.
	currentSourceDigest, err := hashFile(ctx, source, ops)
	if err != nil {
		return fmt.Errorf("mover: re-hash source %q: %w", source, err)
	}
	if currentSourceDigest != sourceDigest {
		return checksumError(source, destination, sourceDigest, currentSourceDigest)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ops.remove(source); err != nil {
		return fmt.Errorf("mover: remove source %q: %w", source, err)
	}
	destinationOwned = false

	emitProgress(cb, total, total, time.Now())
	return nil
}

func checksumError(source, destination string, expected, actual [sha256.Size]byte) error {
	return fmt.Errorf("%w: %s (%s) != %s (%s)", ErrChecksumMismatch,
		source, hex.EncodeToString(expected[:]), destination, hex.EncodeToString(actual[:]))
}

func destinationAbsent(ops *moveOps, destination string) error {
	_, err := ops.lstat(destination)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("mover: inspect destination %q: %w", destination, err)
}

func partialPath(destination string) string {
	directory, name := filepath.Split(destination)
	return filepath.Join(directory, "."+name+".partial")
}

func hashFile(ctx context.Context, name string, ops *moveOps) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	file, err := ops.open(name)
	if err != nil {
		return digest, err
	}
	hasher := sha256.New()
	_, readErr := copyWithContext(ctx, hasher, file, nil)
	closeErr := file.Close()
	if readErr != nil {
		return digest, readErr
	}
	if closeErr != nil {
		return digest, closeErr
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

// copyWithContext is deliberately implemented as a small loop instead of
// io.Copy so cancellation is checked before each read and write.  It also
// handles short writes and reports only bytes that reached the destination.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader, report func(int64)) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	buffer := make([]byte, 1024*1024)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written := 0
			for written < n {
				if err := ctx.Err(); err != nil {
					return done, err
				}
				count, writeErr := dst.Write(buffer[written:n])
				if count > 0 {
					written += count
					done += int64(count)
					if report != nil {
						report(done)
					}
				}
				if writeErr != nil {
					return done, writeErr
				}
				if count == 0 {
					return done, io.ErrShortWrite
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return done, nil
			}
			return done, readErr
		}
	}
}

func emitProgress(cb ProgressFunc, done, total int64, at time.Time) {
	if cb == nil {
		return
	}
	if done < 0 {
		done = 0
	}
	if total < 0 {
		total = 0
	}
	if done > total {
		done = total
	}
	cb(model.Progress{
		BytesDone:  done,
		BytesTotal: total,
		At:         at,
	})
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil && !directorySyncUnsupported(syncErr) {
		return syncErr
	}
	return closeErr
}

func directorySyncUnsupported(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "invalid argument") ||
		strings.Contains(message, "not supported") ||
		strings.Contains(message, "function not implemented")
}

// MoveBatch moves each source into destinationDir using the source basename.
// It preflights all destinations, then moves files sequentially.  Progress is
// reported against the aggregate byte total, so it never moves backwards when
// one file finishes and the next begins.
func MoveBatch(ctx context.Context, sources []string, destinationDir string, progress ...ProgressFunc) error {
	return New().MoveBatch(ctx, sources, destinationDir, progress...)
}

// MoveFiles is an alias for MoveBatch.
func MoveFiles(ctx context.Context, sources []string, destinationDir string, progress ...ProgressFunc) error {
	return MoveBatch(ctx, sources, destinationDir, progress...)
}

// MoveBatch moves each source into destinationDir using this Mover.
func (m *Mover) MoveBatch(ctx context.Context, sources []string, destinationDir string, progress ...ProgressFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if destinationDir == "" {
		return errors.New("mover: destination directory must not be empty")
	}
	ops := m.operations()
	directoryInfo, err := ops.lstat(destinationDir)
	if err != nil {
		return fmt.Errorf("mover: stat destination directory %q: %w", destinationDir, err)
	}
	if !directoryInfo.IsDir() {
		return fmt.Errorf("mover: destination %q is not a directory", destinationDir)
	}

	type batchItem struct {
		source      string
		destination string
		total       int64
	}
	items := make([]batchItem, 0, len(sources))
	destinations := make(map[string]struct{}, len(sources))
	var aggregate int64
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, statErr := ops.lstat(source)
		if statErr != nil {
			return fmt.Errorf("mover: stat source %q: %w", source, statErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mover: source %q is not a regular file", source)
		}
		destination := filepath.Join(destinationDir, filepath.Base(source))
		if _, duplicate := destinations[destination]; duplicate {
			return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
		}
		if err := destinationAbsent(ops, destination); err != nil {
			return err
		}
		total := info.Size()
		if total < 0 {
			total = 0
		}
		items = append(items, batchItem{source: source, destination: destination, total: total})
		destinations[destination] = struct{}{}
		aggregate += total
	}

	cb := progressCallback(progress)
	var offset int64
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		itemOffset := offset
		itemProgress := func(p model.Progress) {
			done := itemOffset + p.BytesDone
			total := aggregate
			if done > total {
				done = total
			}
			emitProgress(cb, done, total, p.At)
		}
		if err := m.Move(ctx, item.source, item.destination, itemProgress); err != nil {
			return err
		}
		offset += item.total
	}
	if cb != nil && len(items) == 0 {
		emitProgress(cb, 0, 0, time.Now())
	}
	return nil
}

// MoveRequest describes one source/destination pair for callers whose batch
// destinations are not all in one directory.
type MoveRequest struct {
	Source      string
	Destination string
}

// MoveRequests moves explicit source/destination pairs in order.  It shares
// the same no-overwrite and aggregate-progress guarantees as MoveBatch.
func MoveRequests(ctx context.Context, requests []MoveRequest, progress ...ProgressFunc) error {
	return New().MoveRequests(ctx, requests, progress...)
}

// MoveRequests moves explicit source/destination pairs using this Mover.
func (m *Mover) MoveRequests(ctx context.Context, requests []MoveRequest, progress ...ProgressFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ops := m.operations()
	totals := make([]int64, len(requests))
	var aggregate int64
	destinations := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		info, err := ops.lstat(request.Source)
		if err != nil {
			return fmt.Errorf("mover: stat source %q: %w", request.Source, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mover: source %q is not a regular file", request.Source)
		}
		if _, exists := destinations[request.Destination]; exists {
			return fmt.Errorf("%w: %s", ErrDestinationExists, request.Destination)
		}
		if err := destinationAbsent(ops, request.Destination); err != nil {
			return err
		}
		destinations[request.Destination] = struct{}{}
		totals[i] = info.Size()
		if totals[i] < 0 {
			totals[i] = 0
		}
		aggregate += totals[i]
	}

	cb := progressCallback(progress)
	var offset int64
	for i, request := range requests {
		itemOffset := offset
		itemProgress := func(p model.Progress) {
			emitProgress(cb, itemOffset+p.BytesDone, aggregate, p.At)
		}
		if err := m.Move(ctx, request.Source, request.Destination, itemProgress); err != nil {
			return err
		}
		offset += totals[i]
	}
	if cb != nil && len(requests) == 0 {
		emitProgress(cb, 0, 0, time.Now())
	}
	return nil
}
