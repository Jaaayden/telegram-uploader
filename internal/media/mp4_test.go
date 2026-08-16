package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/jayden/telegram-video-uploader/internal/model"
)

func TestParseMP4MetadataFastStart(t *testing.T) {
	path := writeFixture(t, newProgressiveFile(t, true, true))

	metadata, err := ParseMP4Metadata(path)
	if err != nil {
		t.Fatalf("ParseMP4Metadata() error = %v", err)
	}
	want := model.VideoMetadata{
		DurationSeconds:   3,
		Width:             1920,
		Height:            1080,
		SupportsStreaming: true,
	}
	if metadata != want {
		t.Fatalf("ParseMP4Metadata() = %+v, want %+v", metadata, want)
	}
}

func TestParseMP4MetadataMoovAfterMdat(t *testing.T) {
	path := writeFixture(t, newProgressiveFile(t, false, true))

	metadata, err := ParseMP4Metadata(path)
	if err != nil {
		t.Fatalf("ParseMP4Metadata() error = %v", err)
	}
	if metadata.SupportsStreaming {
		t.Fatalf("SupportsStreaming = true, want false for moov after mdat")
	}
}

func TestParseMP4MetadataNoVideoTrack(t *testing.T) {
	path := writeFixture(t, newProgressiveFile(t, true, false))

	_, err := ParseMP4Metadata(path)
	if !errors.Is(err, ErrNoVideoTrack) {
		t.Fatalf("ParseMP4Metadata() error = %v, want ErrNoVideoTrack", err)
	}
	if err == nil || err.Error() != ErrNoVideoTrack.Error() {
		t.Fatalf("error = %v, want clear user-facing no-video error", err)
	}
}

func TestParseMP4MetadataMissingTailSampleAllowsOriginalUploadWithWarning(t *testing.T) {
	path := writeFixture(t, newProgressiveFile(t, true, true))
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) < 4 {
		t.Fatal("fixture unexpectedly too small")
	}
	if err := os.WriteFile(path, contents[:len(contents)-1], 0o600); err != nil {
		t.Fatal(err)
	}

	metadata, err := ParseMP4Metadata(path)
	if err != nil {
		t.Fatalf("ParseMP4Metadata() error = %v, want compatible upload metadata", err)
	}
	if !metadata.TruncatedMediaData {
		t.Fatalf("TruncatedMediaData = false, want visible source warning")
	}
	if metadata.Width != 1920 || metadata.Height != 1080 || metadata.DurationSeconds != 3 {
		t.Fatalf("metadata = %+v, want intact moov metadata", metadata)
	}
}

func TestValidateMdatBoundsRejectsMissingHeader(t *testing.T) {
	mdat := &mp4.MdatBox{StartPos: 100, Data: []byte{0, 1, 2, 3}}
	_, _, _, err := validateMdatBounds([]mp4.Box{mdat}, 50)
	if !errors.Is(err, ErrInvalidMP4) {
		t.Fatalf("validateMdatBounds() error = %v, want ErrInvalidMP4", err)
	}

	mdat.StartPos = 46
	_, _, _, err = validateMdatBounds([]mp4.Box{mdat}, 50)
	if !errors.Is(err, ErrInvalidMP4) {
		t.Fatalf("validateMdatBounds() partial-header error = %v, want ErrInvalidMP4", err)
	}
}

// newProgressiveFile uses only mp4ff boxes, keeping the fixture tiny while
// still exercising the same metadata tree as a real progressive MP4.
func newProgressiveFile(t *testing.T, fastStart, video bool) *mp4.File {
	t.Helper()

	file := mp4.NewFile()
	ftyp := mp4.CreateFtyp()
	moov := mp4.NewMoovBox()
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = 1000
	mvhd.Duration = 2500
	moov.AddChild(mvhd)

	mediaType := "audio"
	if video {
		mediaType = "video"
	}
	track := mp4.CreateEmptyTrak(1, 1000, mediaType, "und")
	track.Mdia.Mdhd.Duration = 2500
	// Give the empty track one tiny sample-table entry so mp4ff treats this
	// fixture as a progressive file instead of a fragmented init segment.
	track.Mdia.Minf.Stbl.Stts.SampleCount = []uint32{1}
	track.Mdia.Minf.Stbl.Stts.SampleTimeDelta = []uint32{2500}
	if err := track.Mdia.Minf.Stbl.Stsc.AddEntry(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	track.Mdia.Minf.Stbl.Stsz.SampleUniformSize = 4
	track.Mdia.Minf.Stbl.Stsz.SampleNumber = 1
	// Reserve one stco entry before calculating moov.Size; its value is filled
	// after the final box order determines the mdat payload position.
	track.Mdia.Minf.Stbl.Stco.ChunkOffset = []uint32{0}
	if video {
		track.Tkhd.Width = mp4.Fixed32(1920 << 16)
		track.Tkhd.Height = mp4.Fixed32(1080 << 16)
	}
	moov.AddChild(track)

	mdat := &mp4.MdatBox{Data: []byte{0x00, 0x01, 0x02, 0x03}}
	file.AddChild(ftyp, 0)
	if fastStart {
		mdatStart := ftyp.Size() + moov.Size()
		track.Mdia.Minf.Stbl.Stco.ChunkOffset = []uint32{uint32(mdatStart + mdat.HeaderSize())}
		file.AddChild(moov, ftyp.Size())
		file.AddChild(mdat, mdatStart)
	} else {
		mdatStart := ftyp.Size()
		track.Mdia.Minf.Stbl.Stco.ChunkOffset = []uint32{uint32(mdatStart + mdat.HeaderSize())}
		file.AddChild(mdat, ftyp.Size())
		file.AddChild(moov, ftyp.Size()+mdat.Size())
	}
	return file
}

func writeFixture(t *testing.T, file *mp4.File) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.mp4")
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Encode(output); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
