package main

import (
	"bufio"
	"fmt"
	"log"
	"time"

	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
)

func durationGoToMPEGTS(v time.Duration) int64 {
	return int64(v.Seconds() * 90000)
}

// mpegtsMuxer allows to save a H265 stream into a MPEG-TS file.
type mpegtsMuxer struct {
	vps []byte
	sps []byte
	pps []byte

	b     *bufio.Writer
	w     *TsWriter
	track *TsTrack
	// codec selection
	isH265 bool

	// DTS extractors
	dtsExtractor265 *h265.DTSExtractor
	dtsExtractor264 *h264.DTSExtractor

	// audio
	aTrack        *TsTrack
	aacSampleHz   int
	aacChannels   int
	aacObjectType int
}

// initialize initializes a mpegtsMuxer.
func (e *mpegtsMuxer) initialize() error {
	if e.isH265 {
		e.track = &TsTrack{Codec: &TsCodecH265{}}
	} else {
		e.track = &TsTrack{Codec: &TsCodecH264{}}
	}
	tracks := []*TsTrack{e.track}
	if e.aacSampleHz > 0 && e.aacChannels > 0 {
		e.aTrack = &TsTrack{Codec: &TsCodecAAC{}}
		tracks = append(tracks, e.aTrack)
	}
	e.w = NewTsWriter(e.b, tracks)
	return nil
}

// close closes all the mpegtsMuxer resources.
func (e *mpegtsMuxer) close() {
	e.b.Flush()
}

// writeH265 writes a H265 access unit into MPEG-TS.
func (e *mpegtsMuxer) writeH265(au [][]byte, pts time.Duration, ntp time.Time, hasNtp bool) error {
	au, isIFrame, isIDRFrame := filterH265AU(au, &e.vps, &e.sps, &e.pps)

	if au == nil {
		log.Printf("Nil AU")
		return nil
	}

	// add VPS, SPS and PPS before random access access unit
	if isIFrame {
		au = append([][]byte{e.vps, e.sps, e.pps}, au...)
	}

	var dts time.Duration

	if e.dtsExtractor265 == nil {
		// skip samples silently until we find one with a IDR
		if !isIFrame {
			log.Printf("Do not send noise")
			return nil
		}
		e.dtsExtractor265 = h265.NewDTSExtractor()
	}

	var err error
	dts, err = e.dtsExtractor265.Extract(au, pts)
	if err != nil {
		return err
	}

	mpegPts := durationGoToMPEGTS(pts)
	mpegDts := durationGoToMPEGTS(dts)

	frameType := ""
	if isIDRFrame {
		frameType = "[IDR]"
	} else if isIFrame {
		frameType = "[I]"
	}

	if isIFrame {
		packetTime := ntp
		if !hasNtp {
			packetTime = time.Now() // fallback to receiver system time
		}
		log.Printf("Write TS packet with pts=%d dts=%d time=%d %s", mpegPts, mpegDts, packetTime.UnixMilli(), frameType)
		return e.w.WriteH265WithTimestamp(e.track, mpegPts, mpegDts, isIDRFrame, au, packetTime)
	} else {
		log.Printf("Write TS packet with pts=%d dts=%d %s", mpegPts, mpegDts, frameType)
		return e.w.WriteH265(e.track, mpegPts, mpegDts, isIDRFrame, au)
	}
}

// writeH264 writes a H264 access unit into MPEG-TS.
func (e *mpegtsMuxer) writeH264(au [][]byte, pts time.Duration, ntp time.Time, hasNtp bool) error {
	au, isIDRFrame := filterH264AU(au, &e.sps, &e.pps)

	if au == nil {
		log.Printf("Nil AU")
		return nil
	}

	// add SPS and PPS before IDR access unit
	if isIDRFrame {
		au = append([][]byte{e.sps, e.pps}, au...)
	}

	var dts time.Duration

	if e.dtsExtractor264 == nil {
		// skip samples silently until we find one with an IDR
		if !isIDRFrame {
			log.Printf("Do not send noise")
			return nil
		}
		e.dtsExtractor264 = h264.NewDTSExtractor()
	}

	var err error
	dts, err = e.dtsExtractor264.Extract(au, pts)
	if err != nil {
		return err
	}

	mpegPts := durationGoToMPEGTS(pts)
	mpegDts := durationGoToMPEGTS(dts)

	if isIDRFrame {
		packetTime := ntp
		if !hasNtp {
			packetTime = time.Now() // fallback to receiver system time
		}
		log.Printf("Write TS packet with pts=%d dts=%d time=%d [IDR-H264]", mpegPts, mpegDts, packetTime.UnixMilli())
		return e.w.WriteH264WithTimestamp(e.track, mpegPts, mpegDts, true, au, packetTime)
	} else {
		log.Printf("Write TS packet with pts=%d dts=%d [H264]", mpegPts, mpegDts)
		return e.w.WriteH264(e.track, mpegPts, mpegDts, false, au)
	}
}

// writeAACFrames writes AAC frames as ADTS+payload PES, one frame per PES.
func (e *mpegtsMuxer) writeAACFrames(frames [][]byte, basePTS time.Duration) error {
	if e.aTrack == nil || e.aacSampleHz <= 0 {
		return nil
	}
	// AAC LC frame has 1024 samples
	frameDur := time.Duration(float64(time.Second) * float64(1024) / float64(e.aacSampleHz))
	pts := basePTS
	for _, f := range frames {
		adts, err := buildADTSHeader(e.aacObjectType, e.aacSampleHz, e.aacChannels, len(f))
		if err != nil {
			return err
		}
		payload := append(adts, f...)
		mpegPts := durationGoToMPEGTS(pts)
		if err := e.w.writeAudio(e.aTrack, mpegPts, payload); err != nil {
			return err
		}
		pts += frameDur
	}
	return nil
}

// adtsSampleRateTable maps ADTS sampling_frequency_index to sample rate (Hz).
var adtsSampleRateTable = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// buildADTSHeader builds a 7-byte ADTS header (no CRC) for a single AAC frame.
func buildADTSHeader(objType int, sampleRate int, channels int, payloadLen int) ([]byte, error) {
	// ADTS supports ObjectType 1..4 (profile 0..3)
	if objType < 1 || objType > 4 {
		return nil, fmt.Errorf("ADTS only supports ObjectType 1-4, got %d", objType)
	}
	profile := int(objType - 1)

	// Map sample rate to ADTS index
	srTable := adtsSampleRateTable
	srIndex := -1
	for i, v := range srTable {
		if v == sampleRate {
			srIndex = i
			break
		}
	}
	if srIndex < 0 {
		return nil, fmt.Errorf("invalid sample rate: %d", sampleRate)
	}

	// Channel configuration mapping
	var chConf int
	switch {
	case channels >= 1 && channels <= 6:
		chConf = channels
	case channels == 8:
		chConf = 7
	default:
		return nil, fmt.Errorf("invalid channel count (%d)", channels)
	}

	frameLen := payloadLen + 7
	fullness := 0x07FF // same as FFmpeg

	hdr := make([]byte, 7)
	// syncword 0xFFF
	hdr[0] = 0xFF
	hdr[1] = 0xF1 // 1111 0001: sync high + MPEG-4 + layer 00 + no CRC
	hdr[2] = uint8((profile&0x3)<<6 | (srIndex&0x0F)<<2 | ((chConf >> 2) & 0x01))
	hdr[3] = uint8(((chConf & 0x03) << 6) | ((frameLen >> 11) & 0x03))
	hdr[4] = uint8((frameLen >> 3) & 0xFF)
	hdr[5] = uint8(((frameLen & 0x7) << 5) | ((fullness >> 6) & 0x1F))
	hdr[6] = uint8((fullness & 0x3F) << 2) // + 0 raw blocks
	return hdr, nil
}

// parseADTSHeader extracts objectType, sampleRate and channel count from a
// 7-byte (no-CRC) ADTS header, mirroring buildADTSHeader's bit layout.
func parseADTSHeader(hdr []byte) (objType, sampleRate, channels int, err error) {
	if len(hdr) < 7 {
		return 0, 0, 0, fmt.Errorf("ADTS header too short")
	}

	profile := (hdr[2] >> 6) & 0x3
	objType = int(profile) + 1

	srIndex := (hdr[2] >> 2) & 0x0F
	if int(srIndex) >= len(adtsSampleRateTable) {
		return 0, 0, 0, fmt.Errorf("invalid sample rate index: %d", srIndex)
	}
	sampleRate = adtsSampleRateTable[srIndex]

	chConf := ((hdr[2] & 0x01) << 2) | ((hdr[3] >> 6) & 0x03)
	if chConf == 7 {
		channels = 8
	} else {
		channels = int(chConf)
	}

	return objType, sampleRate, channels, nil
}

// writeAudioPES writes raw audio PES payload (with ADTS already present) at given PTS.
func (e *mpegtsMuxer) writeAudioPES(pesData []byte, pts time.Duration) error {
	if e.aTrack == nil {
		return nil
	}
	mpegPts := durationGoToMPEGTS(pts)
	return e.w.writeAudio(e.aTrack, mpegPts, pesData)
}
