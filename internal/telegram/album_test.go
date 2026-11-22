package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

func TestStagedFromMedia(t *testing.T) {
	ref := []byte{1, 2, 3, 4}
	got, err := stagedFromMedia(&tg.MessageMediaDocument{
		Document: &tg.Document{
			ID:            1001,
			AccessHash:    2002,
			FileReference: ref,
		},
	})
	if err != nil {
		t.Fatalf("stagedFromMedia() error = %v", err)
	}
	if got.DocumentID != 1001 || got.AccessHash != 2002 || !bytes.Equal(got.FileReference, ref) {
		t.Fatalf("stagedFromMedia() = %+v", got)
	}
	// Copy must not alias the underlying slice.
	ref[0] = 99
	if got.FileReference[0] != 1 {
		t.Fatal("FileReference aliased source slice")
	}

	if _, err := stagedFromMedia(&tg.MessageMediaDocument{Document: &tg.DocumentEmpty{ID: 1}}); err == nil {
		t.Fatal("expected error for DocumentEmpty")
	}
	if _, err := stagedFromMedia(&tg.MessageMediaEmpty{}); err == nil {
		t.Fatal("expected error for non-document media")
	}
}

func TestAlbumMessageIDs(t *testing.T) {
	items := []AlbumItem{
		{RandomID: 101},
		{RandomID: 202},
		{RandomID: 303},
	}
	// Deliberately returned out of order and missing RandomID 202.
	updates := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 33, RandomID: 303},
			&tg.UpdateMessageID{ID: 11, RandomID: 101},
		},
	}
	got := albumMessageIDs(updates, items)
	if len(got) != 3 || got[0] != 11 || got[1] != 0 || got[2] != 33 {
		t.Fatalf("albumMessageIDs = %v, want [11 0 33]", got)
	}

	// Single-item fallback via UpdateShortSentMessage / unpack.MessageID.
	single := albumMessageIDs(&tg.UpdateShortSentMessage{ID: 77}, []AlbumItem{{RandomID: 999}})
	if len(single) != 1 || single[0] != 77 {
		t.Fatalf("single albumMessageIDs = %v, want [77]", single)
	}
}

func TestStaleAlbumMediaIndex(t *testing.T) {
	wrap := func(code int, msg string) error {
		return fmt.Errorf("%w：%w", ErrAlbumRejected, tgerr.New(code, msg))
	}
	cases := []struct {
		name      string
		err       error
		wantIdx   int
		wantStale bool
	}{
		{name: "indexed expired", err: wrap(400, "FILE_REFERENCE_0_EXPIRED"), wantIdx: 0, wantStale: true},
		{name: "indexed invalid", err: wrap(400, "FILE_REFERENCE_3_INVALID"), wantIdx: 3, wantStale: true},
		{name: "unindexed expired", err: wrap(400, "FILE_REFERENCE_EXPIRED"), wantIdx: -1, wantStale: true},
		{name: "unindexed invalid", err: wrap(400, "FILE_REFERENCE_INVALID"), wantIdx: -1, wantStale: true},
		{name: "media empty", err: wrap(400, "MEDIA_EMPTY"), wantIdx: -1, wantStale: true},
		{name: "document invalid", err: wrap(400, "DOCUMENT_INVALID"), wantIdx: -1, wantStale: true},
		{name: "caption too long", err: wrap(400, "MEDIA_CAPTION_TOO_LONG"), wantIdx: -1, wantStale: false},
		{name: "403 forbidden", err: wrap(403, "CHAT_WRITE_FORBIDDEN"), wantIdx: -1, wantStale: false},
		{name: "non-rejected rpc err", err: tgerr.New(400, "FILE_REFERENCE_0_EXPIRED"), wantIdx: -1, wantStale: false},
		{name: "generic err", err: errors.New("boom"), wantIdx: -1, wantStale: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, stale := StaleAlbumMediaIndex(tc.err)
			if idx != tc.wantIdx || stale != tc.wantStale {
				t.Fatalf("StaleAlbumMediaIndex() = (%d, %t), want (%d, %t)", idx, stale, tc.wantIdx, tc.wantStale)
			}
		})
	}
}

func TestSendAlbumValidation(t *testing.T) {
	client := &Client{}
	channel := model.Channel{ID: 123, AccessHash: 456}
	if _, err := client.SendAlbum(context.Background(), channel, nil); err == nil {
		t.Fatal("expected error for 0 items")
	}
	tooMany := make([]AlbumItem, model.MaxAlbumSize+1)
	for i := range tooMany {
		tooMany[i] = AlbumItem{RandomID: int64(i + 1)}
	}
	if _, err := client.SendAlbum(context.Background(), channel, tooMany); err == nil {
		t.Fatal("expected error for > MaxAlbumSize items")
	}
	if _, err := client.SendAlbum(context.Background(), channel, []AlbumItem{{RandomID: 0}}); err == nil {
		t.Fatal("expected error for zero RandomID")
	}
}
