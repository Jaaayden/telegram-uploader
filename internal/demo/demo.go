// Package demo provides an in-memory Telegram transport and sample videos so
// the interface can be exercised without credentials or network access. It
// backs the --demo flag (previews, screenshots) and the --smoke self-test that
// CI runs against the packaged application.
package demo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

const (
	mib = int64(1) << 20
	// MaxUploadBytes mirrors the usual Bot limit so the oversize flow shows up.
	MaxUploadBytes = 2000 * mib
	// simulatedRate is the pretend link speed when no limit is configured.
	simulatedRate = 24 * mib
	tick          = 100 * time.Millisecond
)

// Channel is the channel the demo binds to.
var Channel = model.Channel{ID: 1000000001, AccessHash: 1, Title: "演示频道"}

// Client simulates uploads at a steady rate and honours the rate limiter.
type Client struct {
	limiter *tgtransport.RateLimiter
	binding chan tgtransport.BindingEvent
	mu      sync.Mutex
	nextID  int
}

// NewClient matches the signature the service uses to build its transport.
func NewClient(config tgtransport.Config, _ tgtransport.Events) *Client {
	return &Client{limiter: config.RateLimiter, binding: make(chan tgtransport.BindingEvent, 1), nextID: 100}
}

func (c *Client) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *Client) WaitReady(ctx context.Context) (tgtransport.Identity, error) {
	select {
	case <-time.After(300 * time.Millisecond):
		return tgtransport.Identity{BotID: 1, Username: "demo_uploader_bot", MaxUploadBytes: MaxUploadBytes, MaxUploadExact: true}, nil
	case <-ctx.Done():
		return tgtransport.Identity{}, ctx.Err()
	}
}

func (c *Client) BeginChannelBinding() (string, error) {
	// A real client waits for the code to appear in the channel; the demo
	// simply confirms after a moment.
	time.AfterFunc(2*time.Second, func() {
		select {
		case c.binding <- tgtransport.BindingEvent{Channel: Channel}:
		default:
		}
	})
	return "TGUP_VERIFY_DEMO0000000000000000", nil
}

func (c *Client) BindingEvents() <-chan tgtransport.BindingEvent { return c.binding }

func (c *Client) ValidateChannel(_ context.Context, channel model.Channel) (model.Channel, error) {
	return channel, nil
}

func (c *Client) SetUploadConcurrency(value int) int { return value }

func (c *Client) UploadVideo(ctx context.Context, request tgtransport.UploadRequest, onProgress func(model.Progress)) (int, error) {
	if request.File == nil {
		return 0, errors.New("demo: missing file")
	}
	info, err := request.File.Stat()
	if err != nil {
		return 0, err
	}
	total := info.Size()
	windowStart := time.Now()
	var done, windowBytes int64
	// Like the real transport, continue a recent partial upload.
	fileID := request.Resume.FileID
	if request.Resume.Usable(total) {
		done = request.Resume.ResumedBytes()
		windowBytes = done
	} else {
		fileID = time.Now().UnixNano()
	}
	rate := float64(simulatedRate)
	step := simulatedRate / int64(time.Second/tick)
	for done < total {
		chunk := min(step, total-done)
		if limit := c.limiter.Limit(); limit > 0 {
			// Keep progress smooth under a low limit: ten slices per second.
			chunk = min(chunk, max(limit/10, 1))
		}
		if err := c.limiter.Wait(ctx, int(chunk)); err != nil {
			return 0, err
		}
		select {
		case <-time.After(tick):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		done += chunk
		if request.OnCheckpoint != nil {
			request.OnCheckpoint(tgtransport.ResumeToken{
				FileID: fileID, Parts: int(done / tgtransport.UploadPartSize),
				PartSize: tgtransport.UploadPartSize, Size: total, At: time.Now(),
			})
		}
		if onProgress != nil {
			// Report the recent rate so a changed limit shows up at once.
			now := time.Now()
			if elapsed := now.Sub(windowStart).Seconds(); elapsed >= 1 {
				rate = float64(done-windowBytes) / elapsed
				windowStart, windowBytes = now, done
			}
			onProgress(model.Progress{BytesDone: done, BytesTotal: total, BytesPerSecond: rate, At: now})
		}
	}
	if request.BeforeSend != nil {
		if err := request.BeforeSend(); err != nil {
			return 0, err
		}
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return id, nil
}

// Secrets keeps credentials in memory so the demo never touches the system
// keychain (which could prompt, or block an unattended CI run).
type Secrets struct {
	mu     sync.Mutex
	values map[string]string
}

// NewSecrets returns a store pre-filled with placeholder credentials.
func NewSecrets() *Secrets {
	return &Secrets{values: map[string]string{"hash": "demo-api-hash", "token": "1:demo-token"}}
}

func (s *Secrets) get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	if !ok {
		return "", credentials.ErrNotFound
	}
	return value, nil
}

func (s *Secrets) set(key, value string) error {
	s.mu.Lock()
	s.values[key] = value
	s.mu.Unlock()
	return nil
}

func (s *Secrets) GetBotToken() (string, error)      { return s.get("token") }
func (s *Secrets) SetBotToken(v string) error        { return s.set("token", v) }
func (s *Secrets) GetAPIHash() (string, error)       { return s.get("hash") }
func (s *Secrets) SetAPIHash(v string) error         { return s.set("hash", v) }
func (s *Secrets) GetProxyPassword() (string, error) { return s.get("proxy") }
func (s *Secrets) SetProxyPassword(v string) error   { return s.set("proxy", v) }
func (s *Secrets) DeleteProxyPassword() error {
	s.mu.Lock()
	delete(s.values, "proxy")
	s.mu.Unlock()
	return nil
}

// Sample is one generated video.
type Sample struct {
	Name string
	Size int64
}

// QueueSamples are queued at startup; FolderSamples wait in a second folder
// for the "add folder" flow.
var (
	QueueSamples = []Sample{
		{"01 开场与课程介绍.mp4", 186 * mib},
		{"02 环境准备.mp4", 412 * mib},
		{"03 第一个示例.mp4", 655 * mib},
		{"04 进阶技巧（上）.mp4", 1310 * mib},
		{"05 进阶技巧（下）.mp4", 2360 * mib},
		{"06 常见问题答疑.mp4", 498 * mib},
		{"07 实战演练.mp4", 920 * mib},
		{"08 总结与后续.mp4", 143 * mib},
	}
	FolderSamples = []Sample{
		{"番外 1 花絮.mp4", 88 * mib},
		{"番外 2 访谈.mp4", 301 * mib},
		{"番外 10 彩蛋.mp4", 57 * mib},
	}
)

// WriteSamples creates sparse MP4 files of the given sizes in dir. They have
// a valid header and take almost no disk space.
func WriteSamples(dir string, samples []Sample) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, sample := range samples {
		if err := writeSparseMP4(filepath.Join(dir, sample.Name), sample.Size); err != nil {
			return fmt.Errorf("demo: write %s: %w", sample.Name, err)
		}
	}
	return nil
}

func writeSparseMP4(path string, size int64) error {
	file := mp4.NewFile()
	ftyp := mp4.CreateFtyp()
	moov := mp4.NewMoovBox()
	mvhd := mp4.CreateMvhd()
	mvhd.Timescale = 1000
	mvhd.Duration = 600_000
	moov.AddChild(mvhd)
	track := mp4.CreateEmptyTrak(1, 1000, "video", "und")
	track.Mdia.Mdhd.Duration = 600_000
	stbl := track.Mdia.Minf.Stbl
	stbl.Stts.SampleCount = []uint32{1}
	stbl.Stts.SampleTimeDelta = []uint32{600_000}
	if err := stbl.Stsc.AddEntry(1, 1, 1); err != nil {
		return err
	}
	stbl.Stsz.SampleUniformSize = 4
	stbl.Stsz.SampleNumber = 1
	stbl.Stco.ChunkOffset = []uint32{0}
	track.Tkhd.Width = mp4.Fixed32(1920 << 16)
	track.Tkhd.Height = mp4.Fixed32(1080 << 16)
	moov.AddChild(track)
	mdat := &mp4.MdatBox{Data: []byte{0, 1, 2, 3}}
	file.AddChild(ftyp, 0)
	mdatStart := ftyp.Size() + moov.Size()
	stbl.Stco.ChunkOffset = []uint32{uint32(mdatStart + mdat.HeaderSize())}
	file.AddChild(moov, ftyp.Size())
	file.AddChild(mdat, mdatStart)

	var encoded bytes.Buffer
	if err := file.Encode(&encoded); err != nil {
		return err
	}
	data := encoded.Bytes()
	if size < int64(len(data)) {
		size = int64(len(data))
	}
	// Stretch the trailing mdat box to the requested size, then let the file
	// system provide the (sparse) payload.
	binary.BigEndian.PutUint32(data[mdatStart:], uint32(size-int64(mdatStart)))
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Truncate(size); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
