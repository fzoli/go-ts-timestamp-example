package main

import (
	"fmt"
	"time"

	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/pkg/formats/fmp4"
)

// fmp4VideoTimescale is the timescale (units per second) used for the video
// track of every fMP4 fragment. Audio uses its own sample rate as timescale.
const fmp4VideoTimescale = 90000

func fmp4Ticks(v time.Duration, timescale int) int64 {
	return int64(v.Seconds() * float64(timescale))
}

// fmp4GopMuxer buffers one GOP (a video keyframe plus all following
// non-keyframes, and any interleaved audio) and marshals it into a single
// fMP4 fragment (moof+mdat), i.e. one HLS .m4s segment.
type fmp4GopMuxer struct {
	vps    []byte
	sps    []byte
	pps    []byte
	isH265 bool

	videoTrackID int
	audioTrackID int
	aacSampleHz  int

	dtsExtractor265 *h265.DTSExtractor
	dtsExtractor264 *h264.DTSExtractor

	videoSamples []*fmp4.PartSample
	videoDTS     []int64 // ticks (fmp4VideoTimescale), parallel to videoSamples
	startDTS     int64
	haveStartDTS bool

	audioSamples    []*fmp4.PartSample
	audioStartTicks int64
	haveAudioStart  bool
}

// writeH265 adds a H265 access unit to the current fragment.
func (e *fmp4GopMuxer) writeH265(au [][]byte, pts time.Duration) error {
	au, isIFrame, _ := filterH265AU(au, &e.vps, &e.sps, &e.pps)
	if au == nil {
		return nil
	}

	if isIFrame {
		au = append([][]byte{e.vps, e.sps, e.pps}, au...)
	}

	if e.dtsExtractor265 == nil {
		if !isIFrame {
			return nil
		}
		e.dtsExtractor265 = h265.NewDTSExtractor()
	}

	dts, err := e.dtsExtractor265.Extract(au, pts)
	if err != nil {
		return err
	}

	sample, err := fmp4.NewPartSampleH265(int32(fmp4Ticks(pts-dts, fmp4VideoTimescale)), au)
	if err != nil {
		return err
	}
	e.appendVideoSample(sample, dts)
	return nil
}

// writeH264 adds a H264 access unit to the current fragment.
func (e *fmp4GopMuxer) writeH264(au [][]byte, pts time.Duration) error {
	au, isIDRFrame := filterH264AU(au, &e.sps, &e.pps)
	if au == nil {
		return nil
	}

	if isIDRFrame {
		au = append([][]byte{e.sps, e.pps}, au...)
	}

	if e.dtsExtractor264 == nil {
		if !isIDRFrame {
			return nil
		}
		e.dtsExtractor264 = h264.NewDTSExtractor()
	}

	dts, err := e.dtsExtractor264.Extract(au, pts)
	if err != nil {
		return err
	}

	sample, err := fmp4.NewPartSampleH264(int32(fmp4Ticks(pts-dts, fmp4VideoTimescale)), au)
	if err != nil {
		return err
	}
	e.appendVideoSample(sample, dts)
	return nil
}

func (e *fmp4GopMuxer) appendVideoSample(sample *fmp4.PartSample, dts time.Duration) {
	dtsTicks := fmp4Ticks(dts, fmp4VideoTimescale)
	if !e.haveStartDTS {
		e.startDTS = dtsTicks
		e.haveStartDTS = true
	}
	e.videoSamples = append(e.videoSamples, sample)
	e.videoDTS = append(e.videoDTS, dtsTicks)
}

// writeAudioPES adds one ADTS-framed AAC frame (as produced by
// mpegtsMuxer.writeAACFrames / carried as-is over SRT/MPEG-TS) to the
// current fragment, stripping the 7-byte ADTS header since fMP4 samples
// carry raw AAC payloads.
func (e *fmp4GopMuxer) writeAudioPES(pes []byte, pts time.Duration) error {
	if e.aacSampleHz <= 0 || len(pes) <= 7 {
		return nil
	}
	raw := pes[7:]

	if !e.haveAudioStart {
		e.audioStartTicks = fmp4Ticks(pts, e.aacSampleHz)
		e.haveAudioStart = true
	}

	// AAC LC: 1024 samples/frame == 1024 ticks in a timescale == sample rate.
	e.audioSamples = append(e.audioSamples, &fmp4.PartSample{
		Duration: 1024,
		Payload:  append([]byte(nil), raw...),
	})
	return nil
}

// finalize computes sample durations and builds the fMP4 Part (fragment) for
// this GOP. nextVideoDTSTicks, if non-nil, is the DTS (in fmp4VideoTimescale
// ticks) of the first sample of the following fragment, used to derive an
// accurate duration for this fragment's last video sample.
func (e *fmp4GopMuxer) finalize(seq uint32, nextVideoDTSTicks *int64) (*fmp4.Part, error) {
	n := len(e.videoSamples)
	if n == 0 {
		return nil, fmt.Errorf("no video samples in GOP")
	}

	for i := 0; i < n; i++ {
		var dur int64
		switch {
		case i < n-1:
			dur = e.videoDTS[i+1] - e.videoDTS[i]
		case nextVideoDTSTicks != nil:
			dur = *nextVideoDTSTicks - e.videoDTS[i]
		case n > 1:
			dur = e.videoDTS[i] - e.videoDTS[i-1]
		default:
			dur = fmp4VideoTimescale / 30 // fallback: assume ~30fps
		}
		if dur < 0 {
			dur = 0
		}
		e.videoSamples[i].Duration = uint32(dur)
	}

	part := &fmp4.Part{
		SequenceNumber: seq,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       e.videoTrackID,
				BaseTime: uint64(e.startDTS),
				Samples:  e.videoSamples,
			},
		},
	}

	if len(e.audioSamples) > 0 {
		part.Tracks = append(part.Tracks, &fmp4.PartTrack{
			ID:       e.audioTrackID,
			BaseTime: uint64(e.audioStartTicks),
			Samples:  e.audioSamples,
		})
	}

	return part, nil
}
