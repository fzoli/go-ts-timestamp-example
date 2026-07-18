package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/pkg/formats/fmp4"
)

type HLSSegment struct {
	Seq             int
	Filename        string
	Duration        float64 // seconds
	ProgramDateTime time.Time
	InitURI         string // init segment this fragment was built against
}

// HLSSegmenter creates fMP4 fragments starting on keyframes (one GOP == one
// .m4s file) and serves a sliding CMAF-style playlist referencing a single
// init.mp4.
type HLSSegmenter struct {
	dir    string
	window int

	mu       sync.RWMutex
	segments []HLSSegment
	nextSeq  int

	// current fragment state
	curGop      *fmp4GopMuxer
	curSeq      int
	curStartPTS time.Duration
	curLastPTS  time.Duration
	curStartPDT time.Time
	curInitURI  string

	// codec params for (re)initialization
	isH265        bool
	vps           []byte
	sps           []byte
	pps           []byte
	aacPresent    bool
	aacSampleHz   int
	aacChannels   int
	aacObjectType int

	// fMP4 init segment(s). Rewritten (as a new, versioned file) whenever
	// the codec params above change compared to the last one written.
	initWritten    bool
	initVersion    int
	currentInitURI string
	videoTrackID   int
	audioTrackID   int

	// snapshot of the params the currently active init segment was built from
	lastInitVPS           []byte
	lastInitSPS           []byte
	lastInitPPS           []byte
	lastInitIsH265        bool
	lastInitAACPresent    bool
	lastInitAACSampleHz   int
	lastInitAACChannels   int
	lastInitAACObjectType int
}

func NewHLSSegmenter(dir string, window int) (*HLSSegmenter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &HLSSegmenter{
		dir:      dir,
		window:   window,
		segments: make([]HLSSegment, 0, window),
		nextSeq:  0,
	}, nil
}

func (h *HLSSegmenter) SetVideoParams(vps, sps, pps []byte, isH265 bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// shallow copy to avoid external mutation
	if vps != nil {
		h.vps = append([]byte(nil), vps...)
	} else {
		h.vps = nil
	}
	if sps != nil {
		h.sps = append([]byte(nil), sps...)
	} else {
		h.sps = nil
	}
	if pps != nil {
		h.pps = append([]byte(nil), pps...)
	} else {
		h.pps = nil
	}
	h.isH265 = isH265
}

func (h *HLSSegmenter) SetAudioParams(sampleHz, channels, objectType int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.aacPresent = sampleHz > 0 && channels > 0
	h.aacSampleHz = sampleHz
	h.aacChannels = channels
	h.aacObjectType = objectType
}

// initParamsChangedLocked reports whether the codec params currently set
// differ from the ones the active init segment was built from.
func (h *HLSSegmenter) initParamsChangedLocked() bool {
	if !h.initWritten {
		return true
	}
	if !bytes.Equal(h.vps, h.lastInitVPS) ||
		!bytes.Equal(h.sps, h.lastInitSPS) ||
		!bytes.Equal(h.pps, h.lastInitPPS) ||
		h.isH265 != h.lastInitIsH265 ||
		h.aacPresent != h.lastInitAACPresent {
		return true
	}
	if h.aacPresent &&
		(h.aacSampleHz != h.lastInitAACSampleHz ||
			h.aacChannels != h.lastInitAACChannels ||
			h.aacObjectType != h.lastInitAACObjectType) {
		return true
	}
	return false
}

// ensureInitLocked (re)writes a versioned init segment (ftyp+moov) whenever
// valid codec params are available and differ from the currently active
// init. Called with h.mu held.
func (h *HLSSegmenter) ensureInitLocked() error {
	if len(h.sps) == 0 || len(h.pps) == 0 || (h.isH265 && len(h.vps) == 0) {
		return nil // wait for params
	}
	if !h.initParamsChangedLocked() {
		return nil
	}

	h.videoTrackID = 1
	var videoCodec fmp4.Codec
	if h.isH265 {
		videoCodec = &fmp4.CodecH265{VPS: h.vps, SPS: h.sps, PPS: h.pps}
	} else {
		videoCodec = &fmp4.CodecH264{SPS: h.sps, PPS: h.pps}
	}
	tracks := []*fmp4.InitTrack{
		{ID: h.videoTrackID, TimeScale: fmp4VideoTimescale, Codec: videoCodec},
	}

	if h.aacPresent {
		h.audioTrackID = 2
		tracks = append(tracks, &fmp4.InitTrack{
			ID:        h.audioTrackID,
			TimeScale: uint32(h.aacSampleHz),
			Codec: &fmp4.CodecMPEG4Audio{
				Config: mpeg4audio.Config{
					Type:         mpeg4audio.ObjectType(h.aacObjectType),
					SampleRate:   h.aacSampleHz,
					ChannelCount: h.aacChannels,
				},
			},
		})
	}

	init := &fmp4.Init{Tracks: tracks}
	filename := fmt.Sprintf("init-%d.mp4", h.initVersion)
	f, err := os.Create(filepath.Join(h.dir, filename))
	if err != nil {
		return err
	}
	marshalErr := init.Marshal(f)
	closeErr := f.Close()
	if marshalErr != nil {
		return marshalErr
	}
	if closeErr != nil {
		return closeErr
	}

	h.initVersion++
	h.initWritten = true
	h.currentInitURI = filename
	h.lastInitVPS = append([]byte(nil), h.vps...)
	h.lastInitSPS = append([]byte(nil), h.sps...)
	h.lastInitPPS = append([]byte(nil), h.pps...)
	h.lastInitIsH265 = h.isH265
	h.lastInitAACPresent = h.aacPresent
	h.lastInitAACSampleHz = h.aacSampleHz
	h.lastInitAACChannels = h.aacChannels
	h.lastInitAACObjectType = h.aacObjectType
	return nil
}

// StartSegment forcibly closes any open fragment and starts a new one at
// given PTS and wall-clock time.
func (h *HLSSegmenter) StartSegment(firstPTS time.Duration, ntp time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.ensureInitLocked(); err != nil {
		return err
	}

	nextTicks := fmp4Ticks(firstPTS, fmp4VideoTimescale)
	if err := h.closeLocked(&nextTicks); err != nil {
		return err
	}

	seq := h.nextSeq
	h.nextSeq++

	h.curSeq = seq
	h.curStartPTS = firstPTS
	h.curLastPTS = firstPTS
	h.curInitURI = h.currentInitURI
	if ntp.IsZero() {
		ntp = time.Now()
	}
	h.curStartPDT = ntp

	h.curGop = &fmp4GopMuxer{
		vps:          h.vps,
		sps:          h.sps,
		pps:          h.pps,
		isH265:       h.isH265,
		videoTrackID: h.videoTrackID,
		audioTrackID: h.audioTrackID,
		aacSampleHz:  h.aacSampleHz,
	}
	return nil
}

// segmentDurationSec returns current fragment duration in seconds based on video PTS.
func (h *HLSSegmenter) segmentDurationSec() float64 {
	d := h.curLastPTS - h.curStartPTS
	if d < 0 {
		d = 0
	}
	return d.Seconds()
}

// CloseCurrent finalizes and indexes the current fragment.
func (h *HLSSegmenter) CloseCurrent() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closeLocked(nil)
}

func (h *HLSSegmenter) closeLocked(nextVideoDTSTicks *int64) error {
	if h.curGop == nil {
		return nil
	}

	part, err := h.curGop.finalize(uint32(h.curSeq+1), nextVideoDTSTicks)
	h.curGop = nil
	if err != nil {
		// no video samples were collected for this GOP; drop it silently
		return nil
	}

	filename := fmt.Sprintf("seg-%d.m4s", h.curSeq)
	path := filepath.Join(h.dir, filename)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	marshalErr := part.Marshal(f)
	closeErr := f.Close()
	if marshalErr != nil {
		return marshalErr
	}
	if closeErr != nil {
		return closeErr
	}

	dur := h.segmentDurationSec()
	seg := HLSSegment{
		Seq:             h.curSeq,
		Filename:        filename,
		Duration:        dur,
		ProgramDateTime: h.curStartPDT,
		InitURI:         h.curInitURI,
	}
	h.segments = append(h.segments, seg)
	// slide window and delete old files
	removedInitURIs := map[string]bool{}
	for len(h.segments) > h.window {
		old := h.segments[0]
		h.segments = h.segments[1:]
		_ = os.Remove(filepath.Join(h.dir, old.Filename))
		removedInitURIs[old.InitURI] = true
	}
	// clean up init segments no longer referenced by any fragment in the window
	if len(removedInitURIs) > 0 {
		stillUsed := map[string]bool{h.currentInitURI: true}
		for _, s := range h.segments {
			stillUsed[s.InitURI] = true
		}
		for uri := range removedInitURIs {
			if !stillUsed[uri] {
				_ = os.Remove(filepath.Join(h.dir, uri))
			}
		}
	}
	return nil
}

// WriteVideo writes a video AU to current fragment; drops until a fragment is started.
func (h *HLSSegmenter) WriteVideo(au [][]byte, pts time.Duration, ntp time.Time, hasNtp bool, isH265 bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.curGop == nil {
		return nil
	}
	h.curLastPTS = pts
	if isH265 {
		return h.curGop.writeH265(au, pts)
	}
	return h.curGop.writeH264(au, pts)
}

// WriteAudioPES writes raw audio PES (with ADTS) at given PTS into current fragment.
func (h *HLSSegmenter) WriteAudioPES(pes []byte, pts time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.curGop == nil {
		return nil
	}
	return h.curGop.writeAudioPES(pes, pts)
}

// Playlist returns a dynamic M3U8 playlist string.
func (h *HLSSegmenter) Playlist() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	// compute target duration among available segments (at least 1)
	maxDur := 1.0
	if len(h.segments) > 0 {
		maxDur = 0
		for _, s := range h.segments {
			if s.Duration > maxDur {
				maxDur = s.Duration
			}
		}
	}
	target := int(math.Ceil(maxDur))
	seq := 0
	if len(h.segments) > 0 {
		seq = h.segments[0].Seq
	}
	// build playlist
	b := &bytesBuffer{}
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", seq))
	lastInitURI := ""
	for _, s := range h.segments {
		if s.InitURI != lastInitURI {
			if lastInitURI != "" {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			b.WriteString(`#EXT-X-MAP:URI="` + s.InitURI + `"` + "\n")
			lastInitURI = s.InitURI
		}
		if !s.ProgramDateTime.IsZero() {
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:" + s.ProgramDateTime.UTC().Format(time.RFC3339Nano) + "\n")
		}
		// duration with 3 decimals
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", s.Duration))
		b.WriteString(s.Filename + "\n")
	}
	return b.String()
}

// bytesBuffer is a tiny growable buffer to avoid importing bytes for one use.
type bytesBuffer struct{ data []byte }

func (b *bytesBuffer) WriteString(s string) { b.data = append(b.data, s...) }
func (b *bytesBuffer) String() string       { return string(b.data) }
