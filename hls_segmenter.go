package main

import (
    "bufio"
    "fmt"
    "math"
    "os"
    "path/filepath"
    "sync"
    "time"
)

type HLSSegment struct {
    Seq              int
    Filename         string
    Duration         float64 // seconds
    ProgramDateTime  time.Time
}

// HLSSegmenter creates MPEG-TS segments starting on keyframes and serves a sliding playlist.
type HLSSegmenter struct {
    dir            string
    window         int

    mu             sync.RWMutex
    segments       []HLSSegment
    nextSeq        int

    // current segment state
    curFile        *os.File
    curMux         *mpegtsMuxer
    curSeq         int
    curStartPTS    time.Duration
    curLastPTS     time.Duration
    curStartPDT    time.Time

    // codec params for (re)initialization
    isH265         bool
    vps            []byte
    sps            []byte
    pps            []byte
    aacPresent     bool
    aacSampleHz    int
    aacChannels    int
    aacObjectType  int
}

func NewHLSSegmenter(dir string, window int) (*HLSSegmenter, error) {
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return nil, err
    }
    return &HLSSegmenter{
        dir:     dir,
        window:  window,
        segments: make([]HLSSegment, 0, window),
        nextSeq:  0,
    }, nil
}

func (h *HLSSegmenter) SetVideoParams(vps, sps, pps []byte, isH265 bool) {
    h.mu.Lock()
    defer h.mu.Unlock()
    // shallow copy to avoid external mutation
    if vps != nil { h.vps = append([]byte(nil), vps...) } else { h.vps = nil }
    if sps != nil { h.sps = append([]byte(nil), sps...) } else { h.sps = nil }
    if pps != nil { h.pps = append([]byte(nil), pps...) } else { h.pps = nil }
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

// StartSegment forcibly closes any open segment and starts a new one at given PTS and wall-clock time.
func (h *HLSSegmenter) StartSegment(firstPTS time.Duration, ntp time.Time) error {
    h.mu.Lock()
    defer h.mu.Unlock()
    // close any current
    if err := h.closeLocked(); err != nil {
        return err
    }

    seq := h.nextSeq
    h.nextSeq++
    filename := fmt.Sprintf("seg-%d.ts", seq)
    path := filepath.Join(h.dir, filename)
    f, err := os.Create(path)
    if err != nil {
        return err
    }
    mux := &mpegtsMuxer{
        vps:    h.vps,
        sps:    h.sps,
        pps:    h.pps,
        isH265: h.isH265,
        b:      bufio.NewWriterSize(f, bufferSize),
    }
    if h.aacPresent {
        mux.aacSampleHz = h.aacSampleHz
        mux.aacChannels = h.aacChannels
        mux.aacObjectType = h.aacObjectType
    }
    if err := mux.initialize(); err != nil {
        _ = f.Close()
        return err
    }

    h.curFile = f
    h.curMux = mux
    h.curSeq = seq
    h.curStartPTS = firstPTS
    h.curLastPTS = firstPTS
    if ntp.IsZero() { ntp = time.Now() }
    h.curStartPDT = ntp
    return nil
}

// segmentDurationSec returns current segment duration in seconds based on video PTS.
func (h *HLSSegmenter) segmentDurationSec() float64 {
    d := h.curLastPTS - h.curStartPTS
    if d < 0 {
        d = 0
    }
    return float64(d) / 90000.0
}

// CloseCurrent finalizes and indexes the current segment.
func (h *HLSSegmenter) CloseCurrent() error {
    h.mu.Lock()
    defer h.mu.Unlock()
    return h.closeLocked()
}

func (h *HLSSegmenter) closeLocked() error {
    if h.curMux == nil {
        return nil
    }
    h.curMux.close()
    if h.curFile != nil {
        _ = h.curFile.Close()
    }
    dur := h.segmentDurationSec()
    seg := HLSSegment{
        Seq:             h.curSeq,
        Filename:        fmt.Sprintf("seg-%d.ts", h.curSeq),
        Duration:        dur,
        ProgramDateTime: h.curStartPDT,
    }
    h.segments = append(h.segments, seg)
    // slide window and delete old files
    for len(h.segments) > h.window {
        old := h.segments[0]
        h.segments = h.segments[1:]
        _ = os.Remove(filepath.Join(h.dir, old.Filename))
    }
    h.curFile = nil
    h.curMux = nil
    return nil
}

// WriteVideo writes a video AU to current segment; drops until a segment is started.
func (h *HLSSegmenter) WriteVideo(au [][]byte, pts time.Duration, ntp time.Time, hasNtp bool, isH265 bool) error {
    h.mu.Lock()
    defer h.mu.Unlock()
    if h.curMux == nil {
        return nil
    }
    h.curLastPTS = pts
    if isH265 {
        return h.curMux.writeH265(au, pts, ntp, hasNtp)
    }
    return h.curMux.writeH264(au, pts, ntp, hasNtp)
}

// WriteAudioPES writes raw audio PES (with ADTS) at given PTS into current segment.
func (h *HLSSegmenter) WriteAudioPES(pes []byte, pts time.Duration) error {
    h.mu.Lock()
    defer h.mu.Unlock()
    if h.curMux == nil {
        return nil
    }
    return h.curMux.writeAudioPES(pes, pts)
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
    b.WriteString("#EXT-X-VERSION:3\n")
    b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
    b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
    b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", seq))
    for _, s := range h.segments {
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
type bytesBuffer struct { data []byte }
func (b *bytesBuffer) WriteString(s string) { b.data = append(b.data, s...) }
func (b *bytesBuffer) String() string { return string(b.data) }

