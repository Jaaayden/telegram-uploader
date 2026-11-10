package telegram

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// partStore plays Telegram's side: it remembers parts by file ID and can fail
// or refuse on demand.
type partStore struct {
	mu       sync.Mutex
	parts    map[int64]map[int][]byte
	total    map[int64]int
	calls    int
	failAt   int   // fail the call with this 1-based number (0 = never)
	failWith error // the error for failAt
	refuse   int   // answer "not saved" this many times first
}

func (s *partStore) UploadSaveBigFilePart(_ context.Context, request *tg.UploadSaveBigFilePartRequest) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failAt != 0 && s.calls == s.failAt {
		return false, s.failWith
	}
	if s.refuse > 0 {
		s.refuse--
		return false, nil
	}
	if s.parts == nil {
		s.parts = map[int64]map[int][]byte{}
		s.total = map[int64]int{}
	}
	if s.parts[request.FileID] == nil {
		s.parts[request.FileID] = map[int][]byte{}
	}
	s.parts[request.FileID][request.FilePart] = append([]byte(nil), request.Bytes...)
	s.total[request.FileID] = request.FileTotalParts
	return true, nil
}

// assemble returns the stored file and whether every part is present.
func (s *partStore) assemble(fileID int64) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for part := 0; part < s.total[fileID]; part++ {
		data, ok := s.parts[fileID][part]
		if !ok {
			return nil, false
		}
		out = append(out, data...)
	}
	return out, s.total[fileID] > 0
}

func testPayload(size int) []byte {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index*31 + index>>9)
	}
	return data
}

func newTestUpload(store *partStore, data []byte, partSize, threads int) (*resumableUpload, *[]ResumeToken) {
	var mu sync.Mutex
	checkpoints := &[]ResumeToken{}
	return &resumableUpload{
		api: store, file: bytes.NewReader(data), name: "video.mp4", size: int64(len(data)),
		partSize: partSize, threads: threads,
		checkpoint: func(token ResumeToken) {
			mu.Lock()
			*checkpoints = append(*checkpoints, token)
			mu.Unlock()
		},
	}, checkpoints
}

func TestResumableUploadSendsEveryPartOnce(t *testing.T) {
	const partSize = 1024
	data := testPayload(10*partSize + 300) // a short last part
	store := &partStore{}
	upload, checkpoints := newTestUpload(store, data, partSize, 4)
	var lastProgress int64
	var progressMu sync.Mutex
	upload.progress = func(_ context.Context, state uploader.ProgressState) error {
		progressMu.Lock()
		lastProgress = max(lastProgress, state.Uploaded)
		progressMu.Unlock()
		return nil
	}
	input, err := upload.run(context.Background(), ResumeToken{})
	if err != nil {
		t.Fatal(err)
	}
	big, ok := input.(*tg.InputFileBig)
	if !ok || big.Parts != 11 || big.Name != "video.mp4" || big.ID == 0 {
		t.Fatalf("unexpected input file: %#v", input)
	}
	stored, complete := store.assemble(big.ID)
	if !complete || !bytes.Equal(stored, data) {
		t.Fatal("the stored parts do not reassemble into the file")
	}
	if store.calls != 11 {
		t.Fatalf("%d save calls for 11 parts", store.calls)
	}
	if lastProgress != int64(len(data)) {
		t.Fatalf("final progress = %d, want %d", lastProgress, len(data))
	}
	final := (*checkpoints)[len(*checkpoints)-1]
	if final.FileID != big.ID || final.Parts != 11 || final.PartSize != partSize || final.Size != int64(len(data)) {
		t.Fatalf("final checkpoint = %+v", final)
	}
}

func TestResumableUploadContinuesAfterFailureWithSameFileID(t *testing.T) {
	const partSize = 1024
	data := testPayload(20 * partSize)
	store := &partStore{failAt: 8, failWith: errors.New("connection reset")}
	// One worker makes "the first seven parts are stored" deterministic.
	upload, checkpoints := newTestUpload(store, data, partSize, 1)
	if _, err := upload.run(context.Background(), ResumeToken{}); err == nil {
		t.Fatal("the injected failure was swallowed")
	}
	token := (*checkpoints)[len(*checkpoints)-1]
	if token.Parts != 7 {
		t.Fatalf("checkpoint after failure covers %d parts, want 7", token.Parts)
	}

	store.failAt = 0
	callsBefore := store.calls
	resumed, _ := newTestUpload(store, data, partSize, 4)
	var first int64 = -1
	resumed.progress = func(_ context.Context, state uploader.ProgressState) error {
		if first < 0 {
			first = state.Uploaded
		}
		return nil
	}
	input, err := resumed.run(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	big := input.(*tg.InputFileBig)
	if big.ID != token.FileID {
		t.Fatal("a resumed upload must keep the file ID")
	}
	if sent := store.calls - callsBefore; sent != 13 {
		t.Fatalf("resume sent %d parts, want only the 13 missing ones", sent)
	}
	if first != 7*partSize {
		t.Fatalf("progress resumed at %d, want %d", first, 7*partSize)
	}
	if stored, complete := store.assemble(big.ID); !complete || !bytes.Equal(stored, data) {
		t.Fatal("the resumed upload does not reassemble into the file")
	}
}

func TestResumableUploadIgnoresStaleOrMismatchedTokens(t *testing.T) {
	const partSize = 1024
	data := testPayload(6 * partSize)
	now := time.Now()
	good := ResumeToken{FileID: 77, Parts: 3, PartSize: partSize, Size: int64(len(data)), At: now}
	if !good.usable(int64(len(data)), partSize, now) {
		t.Fatal("a recent matching token must be usable")
	}
	cases := map[string]ResumeToken{
		"expired":        {FileID: 77, Parts: 3, PartSize: partSize, Size: int64(len(data)), At: now.Add(-ResumeWindow - time.Second)},
		"other size":     {FileID: 77, Parts: 3, PartSize: partSize, Size: int64(len(data)) + 1, At: now},
		"other layout":   {FileID: 77, Parts: 3, PartSize: partSize * 2, Size: int64(len(data)), At: now},
		"too many parts": {FileID: 77, Parts: 99, PartSize: partSize, Size: int64(len(data)), At: now},
		"no timestamp":   {FileID: 77, Parts: 3, PartSize: partSize, Size: int64(len(data))},
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			store := &partStore{}
			upload, _ := newTestUpload(store, data, partSize, 2)
			input, err := upload.run(context.Background(), token)
			if err != nil {
				t.Fatal(err)
			}
			big := input.(*tg.InputFileBig)
			if big.ID == 77 || store.calls != 6 {
				t.Fatalf("token was trusted: id=%d calls=%d", big.ID, store.calls)
			}
		})
	}
}

func TestResumableUploadRetriesRefusedPartsAndStopsOnCancel(t *testing.T) {
	const partSize = 1024
	data := testPayload(4 * partSize)
	store := &partStore{refuse: 3}
	upload, _ := newTestUpload(store, data, partSize, 2)
	input, err := upload.run(context.Background(), ResumeToken{})
	if err != nil {
		t.Fatal(err)
	}
	if stored, complete := store.assemble(input.(*tg.InputFileBig).ID); !complete || !bytes.Equal(stored, data) {
		t.Fatal("refused parts were not sent again")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, _ := newTestUpload(&partStore{}, data, partSize, 2)
	if _, err := cancelled.run(ctx, ResumeToken{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run on a cancelled context = %v", err)
	}
}

func TestResumableUploadHonoursRateLimit(t *testing.T) {
	const partSize = 64 << 10
	data := testPayload(16 * partSize) // 1 MiB
	limiter, clock := newFakeLimiter(256 << 10)
	store := &partStore{}
	upload, _ := newTestUpload(store, data, partSize, 4)
	upload.limiter = limiter
	start := clock.at
	if _, err := upload.run(context.Background(), ResumeToken{}); err != nil {
		t.Fatal(err)
	}
	// 1 MiB at 256 KiB/s is about four seconds of simulated time.
	if elapsed := clock.at.Sub(start); elapsed < 3*time.Second || elapsed > 5*time.Second {
		t.Fatalf("1 MiB at 256 KiB/s took %v of simulated time", elapsed)
	}
}

func TestMissingPartsErrorsAreRecognised(t *testing.T) {
	for _, message := range []string{"FILE_PART_0_MISSING", "FILE_PART_137_MISSING", "FILE_PARTS_INVALID", "FILE_PART_SIZE_CHANGED"} {
		if !isMissingPartsError(tgerr.New(400, message)) {
			t.Errorf("%s must be treated as lost parts", message)
		}
	}
	for _, err := range []error{nil, errors.New("FILE_PART_0_MISSING"), tgerr.New(400, "CHAT_WRITE_FORBIDDEN"), tgerr.New(420, "FLOOD_WAIT_5"), context.Canceled} {
		if isMissingPartsError(err) {
			t.Errorf("%v must not be treated as lost parts", err)
		}
	}
}
