package media

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/jayden/telegram-video-uploader/internal/model"
)

// ErrNoVideoTrack is returned when an MP4 does not contain a video track.
var ErrNoVideoTrack = errors.New("MP4 文件不包含视频轨")

// ErrInvalidMP4 is returned when the MP4 container is too malformed or
// incomplete to obtain trustworthy video metadata.
var ErrInvalidMP4 = errors.New("MP4 文件损坏或已截断")

// ParseMP4Metadata reads the MP4 box metadata needed for a Telegram video.
//
// The file is decoded in mp4ff's lazy mdat mode.  As a result, media payloads
// are skipped rather than copied into memory; only the ftyp/moov metadata is
// decoded.  Duration is rounded up to a whole second so a non-zero remainder
// is never reported as a shorter video.
func ParseMP4Metadata(path string) (model.VideoMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return model.VideoMetadata{}, fmt.Errorf("无法读取 MP4 文件：%w", err)
	}
	defer file.Close()
	return ParseMP4MetadataFile(file)
}

// ParseMP4MetadataFile parses an already-open regular file and rewinds it
// before returning, allowing the same verified handle to be uploaded. This
// avoids reopening a path that could have changed between validation and
// upload.
func ParseMP4MetadataFile(file *os.File) (metadata model.VideoMetadata, retErr error) {
	if file == nil {
		return metadata, errors.New("无法读取 MP4 文件：文件句柄为空")
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return metadata, fmt.Errorf("无法读取 MP4 文件：%w", err)
	}
	if !fileInfo.Mode().IsRegular() {
		return metadata, errors.New("无法读取 MP4 文件：路径不是普通文件")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return metadata, fmt.Errorf("无法读取 MP4 文件：定位文件开头失败：%w", err)
	}
	defer func() {
		if _, err := file.Seek(0, io.SeekStart); retErr == nil && err != nil {
			retErr = fmt.Errorf("无法读取 MP4 文件：重置文件位置失败：%w", err)
		}
	}()

	parsed, err := mp4.DecodeFile(file, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return metadata, fmt.Errorf("%w：%v", ErrInvalidMP4, err)
	}
	if parsed == nil || parsed.Moov == nil {
		return metadata, fmt.Errorf("%w：缺少 moov 元数据", ErrInvalidMP4)
	}

	fileSize := uint64(fileInfo.Size())
	firstMdat, hasMdat, truncatedMdat, err := validateMdatBounds(parsed.Children, fileSize)
	if err != nil {
		return metadata, err
	}

	if parsed.Moov.Mvhd == nil {
		return metadata, fmt.Errorf("%w：缺少 mvhd 元数据", ErrInvalidMP4)
	}

	tracks := parsed.Moov.Traks
	if len(tracks) == 0 && parsed.Moov.Trak != nil {
		tracks = []*mp4.TrakBox{parsed.Moov.Trak}
	}
	for _, track := range tracks {
		if track == nil || track.Mdia == nil || track.Mdia.Hdlr == nil {
			continue
		}
		if track.Mdia.Hdlr.HandlerType != "vide" {
			continue
		}

		if track.Tkhd == nil || track.Mdia.Mdhd == nil {
			return metadata, fmt.Errorf("%w：视频轨缺少必要元数据", ErrInvalidMP4)
		}

		width, height, ok := videoDimensions(track)
		if !ok {
			return metadata, fmt.Errorf("%w：视频轨缺少有效宽高", ErrInvalidMP4)
		}

		durationSeconds, err := durationSeconds(track, parsed.Moov)
		if err != nil {
			return metadata, err
		}

		metadata = model.VideoMetadata{
			DurationSeconds:    durationSeconds,
			Width:              width,
			Height:             height,
			SupportsStreaming:  hasMdat && parsed.Moov.StartPos < firstMdat,
			TruncatedMediaData: truncatedMdat,
		}
		return metadata, nil
	}

	return metadata, ErrNoVideoTrack
}

// ReadMP4Metadata is kept as a descriptive alias for callers that treat the
// probe operation as a file read.
func ReadMP4Metadata(path string) (model.VideoMetadata, error) {
	return ParseMP4Metadata(path)
}

// ParseVideoMetadata is an alias used by callers that do not need to expose
// the container format in their API.
func ParseVideoMetadata(path string) (model.VideoMetadata, error) {
	return ParseMP4Metadata(path)
}

func validateMdatBounds(children []mp4.Box, fileSize uint64) (first uint64, found, truncated bool, err error) {
	for _, child := range children {
		mdat, ok := child.(*mp4.MdatBox)
		if !ok {
			continue
		}

		start := mdat.StartPos
		size := mdat.Size()
		if start > fileSize || mdat.HeaderSize() > fileSize-start {
			return 0, false, false, fmt.Errorf("%w：mdat 头部超出文件范围", ErrInvalidMP4)
		}
		if size > fileSize-start {
			// Some players tolerate an MP4 whose final mdat declaration extends
			// past EOF and play the samples that are still present. This is a
			// genuine source-file truncation, but it does not prevent us from
			// reading trustworthy moov metadata or uploading the original bytes.
			truncated = true
		}
		if !found || start < first {
			first = start
			found = true
		}
	}
	return first, found, truncated, nil
}

func videoDimensions(track *mp4.TrakBox) (width, height int, ok bool) {
	if track.Tkhd != nil {
		width, okWidth := fixed32Pixels(track.Tkhd.Width)
		height, okHeight := fixed32Pixels(track.Tkhd.Height)
		if okWidth && okHeight {
			return width, height, true
		}
	}

	// A few encoders leave tkhd dimensions empty but put the coded dimensions
	// in the visual sample entry.  Use that metadata as a safe fallback.
	if track.Mdia == nil || track.Mdia.Minf == nil || track.Mdia.Minf.Stbl == nil || track.Mdia.Minf.Stbl.Stsd == nil {
		return 0, 0, false
	}
	for _, child := range track.Mdia.Minf.Stbl.Stsd.Children {
		visual, ok := child.(*mp4.VisualSampleEntryBox)
		if !ok || visual.Width == 0 || visual.Height == 0 {
			continue
		}
		return int(visual.Width), int(visual.Height), true
	}
	return 0, 0, false
}

func fixed32Pixels(value mp4.Fixed32) (int, bool) {
	rounded := (uint64(value) + 0x8000) >> 16
	if rounded == 0 || rounded > uint64(^uint(0)>>1) {
		return 0, false
	}
	return int(rounded), true
}

func durationSeconds(track *mp4.TrakBox, moov *mp4.MoovBox) (int, error) {
	mdhd := track.Mdia.Mdhd
	if mdhd.Timescale == 0 {
		return 0, fmt.Errorf("%w：视频轨时间刻度无效", ErrInvalidMP4)
	}

	duration := mdhd.Duration
	timescale := uint64(mdhd.Timescale)
	// mdhd is authoritative for a progressive track.  For files carrying a
	// zero media duration, tkhd may still carry a useful movie-timescale value.
	if duration == 0 && track.Tkhd != nil && track.Tkhd.Duration > 0 && moov.Mvhd != nil && moov.Mvhd.Timescale > 0 {
		duration = track.Tkhd.Duration
		timescale = uint64(moov.Mvhd.Timescale)
	}

	seconds := duration / timescale
	if duration%timescale != 0 {
		seconds++
	}
	if seconds > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w：视频时长超出支持范围", ErrInvalidMP4)
	}
	return int(seconds), nil
}
