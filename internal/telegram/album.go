package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// ErrAlbumRejected means Telegram refused to post an album and said why.
// Such an answer means no message was created, so the videos can be
// uploaded and posted again without duplicating anything.
var ErrAlbumRejected = errors.New("Telegram 拒绝了这组合并发送")

// AlbumItem is one staged video of an album, with the caption and the
// random_id that makes Telegram ignore a repeated submission.
type AlbumItem struct {
	Media    model.StagedMedia
	Caption  string
	RandomID int64
}

// AlbumGateway is implemented by transports that can post several videos as
// one media group. The controller falls back to separate messages when its
// gateway does not implement it.
type AlbumGateway interface {
	// StageVideo uploads a video and attaches it to the channel without
	// posting it. No message exists afterwards, so failures are retryable.
	StageVideo(context.Context, UploadRequest, func(model.Progress)) (model.StagedMedia, error)
	// SendAlbum posts staged videos as one message group, in order, and
	// returns their message IDs (zero where Telegram did not report one).
	// One item is posted as an ordinary single message.
	SendAlbum(context.Context, model.Channel, []AlbumItem) ([]int, error)
}

// SendAlbum posts items as one media group. Errors wrap ErrAlbumRejected when
// Telegram answered with a definite refusal, and ErrSendOutcomeUnknown when
// the request may have been carried out.
func (c *Client) SendAlbum(ctx context.Context, channel model.Channel, items []AlbumItem) ([]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(items) == 0 || len(items) > model.MaxAlbumSize {
		return nil, fmt.Errorf("合并发送需要 1 到 %d 个视频，实际 %d 个", model.MaxAlbumSize, len(items))
	}
	for _, item := range items {
		if item.RandomID == 0 {
			return nil, errors.New("random_id 不能为空")
		}
	}
	requestCtx, release, err := c.beginRequest(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	api, err := c.connectedAPI()
	if err != nil {
		return nil, err
	}
	validated, err := c.validateChannel(requestCtx, channel)
	if err != nil {
		return nil, err
	}
	peer := &tg.InputPeerChannel{ChannelID: validated.ID, AccessHash: validated.AccessHash}
	if err := c.reserveSendSlot(requestCtx); err != nil {
		return nil, err
	}
	// Our own random IDs are essential: they were saved before this request
	// and make a resubmission after an unknown outcome harmless.
	var updates tg.UpdatesClass
	if len(items) == 1 {
		updates, err = api.MessagesSendMedia(requestCtx, &tg.MessagesSendMediaRequest{
			Peer:     peer,
			Media:    inputDocument(items[0].Media),
			Message:  items[0].Caption,
			RandomID: items[0].RandomID,
		})
	} else {
		multi := make([]tg.InputSingleMedia, len(items))
		for index, item := range items {
			multi[index] = tg.InputSingleMedia{
				Media:    inputDocument(item.Media),
				RandomID: item.RandomID,
				Message:  item.Caption,
			}
		}
		updates, err = api.MessagesSendMultiMedia(requestCtx, &tg.MessagesSendMultiMediaRequest{
			Peer:       peer,
			MultiMedia: multi,
		})
	}
	if err != nil {
		if rpcErr, ok := tgerr.As(err); ok && rpcErr.IsCodeOneOf(400, 403) {
			return nil, fmt.Errorf("%w：%w", ErrAlbumRejected, err)
		}
		return nil, fmt.Errorf("%w：提交合并消息失败：%w", ErrSendOutcomeUnknown, err)
	}
	return albumMessageIDs(updates, items), nil
}

func inputDocument(media model.StagedMedia) *tg.InputMediaDocument {
	return &tg.InputMediaDocument{ID: &tg.InputDocument{
		ID:            media.DocumentID,
		AccessHash:    media.AccessHash,
		FileReference: media.FileReference,
	}}
}

// StaleAlbumMediaIndex reports whether err is an ErrAlbumRejected caused by an
// expired or invalid staged media reference. When Telegram identifies which
// item in the MultiMedia array failed (FILE_REFERENCE_<n>_EXPIRED / INVALID),
// it returns that zero-based index and true; when the whole album must be
// re-staged, it returns -1 and true.
func StaleAlbumMediaIndex(err error) (int, bool) {
	if !errors.Is(err, ErrAlbumRejected) {
		return -1, false
	}
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return -1, false
	}
	for _, candidate := range []string{rpcErr.Message, rpcErr.Type} {
		switch candidate {
		case "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID", "MEDIA_EMPTY", "DOCUMENT_INVALID":
			return -1, true
		}
		if rest, ok := strings.CutPrefix(candidate, "FILE_REFERENCE_"); ok {
			for _, suffix := range []string{"_EXPIRED", "_INVALID"} {
				if numText, hasSuffix := strings.CutSuffix(rest, suffix); hasSuffix && numText != "" {
					if idx, convErr := strconv.Atoi(numText); convErr == nil && idx >= 0 {
						return idx, true
					}
				}
			}
		}
	}
	return -1, false
}

// albumMessageIDs maps Telegram's random_id → message ID updates back onto
// items. A posted album without an ID for some item still counts as posted.
func albumMessageIDs(updates tg.UpdatesClass, items []AlbumItem) []int {
	ids := make([]int, len(items))
	var list []tg.UpdateClass
	switch value := updates.(type) {
	case *tg.Updates:
		list = value.Updates
	case *tg.UpdatesCombined:
		list = value.Updates
	}
	byRandom := make(map[int64]int, len(list))
	for _, update := range list {
		if mapped, ok := update.(*tg.UpdateMessageID); ok {
			byRandom[mapped.RandomID] = mapped.ID
		}
	}
	for index, item := range items {
		ids[index] = byRandom[item.RandomID]
	}
	if len(items) == 1 && ids[0] == 0 {
		if id, err := unpack.MessageID(updates, nil); err == nil {
			ids[0] = id
		}
	}
	return ids
}
