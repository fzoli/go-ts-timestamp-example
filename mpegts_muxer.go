package main

import (
    "bufio"
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

    b            *bufio.Writer
    w            *TsWriter
    track        *TsTrack
    // codec selection
    isH265 bool

    // DTS extractors
    dtsExtractor265 *h265.DTSExtractor
    dtsExtractor264 *h264.DTSExtractor

    // audio
    aTrack       *TsTrack
    aacSampleHz  int
    aacChannels  int
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
	var filteredAU [][]byte

	isIFrame := false
	isIDRFrame := false

	for _, nalu := range au {
		typ := h265.NALUType((nalu[0] >> 1) & 0b111111)
		switch typ {
		case h265.NALUType_VPS_NUT:
			e.vps = nalu
			continue

		case h265.NALUType_SPS_NUT:
			e.sps = nalu
			continue

		case h265.NALUType_PPS_NUT:
			e.pps = nalu
			continue

		case h265.NALUType_AUD_NUT:
			continue

		case h265.NALUType_CRA_NUT:
			// CRA is an I-frame, but not a random access point
			isIFrame = true

		case h265.NALUType_IDR_W_RADL, h265.NALUType_IDR_N_LP:
			// IDR is both an I-frame and a random access point
			isIFrame = true
			isIDRFrame = true
		}

		filteredAU = append(filteredAU, nalu)
	}

	au = filteredAU

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
    var filteredAU [][]byte

    isIDRFrame := false

    for _, nalu := range au {
        typ := h264.NALUType(nalu[0] & 0x1F)
        switch typ {
        case h264.NALUTypeSPS:
            e.sps = nalu
            continue
        case h264.NALUTypePPS:
            e.pps = nalu
            continue
        case h264.NALUTypeAccessUnitDelimiter:
            continue
        case h264.NALUTypeIDR:
            isIDRFrame = true
        }
        filteredAU = append(filteredAU, nalu)
    }

    au = filteredAU

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
        adts := buildADTSHeader(e.aacSampleHz, e.aacChannels, len(f))
        payload := append(adts, f...)
        mpegPts := durationGoToMPEGTS(pts)
        if err := e.w.writeAudio(e.aTrack, mpegPts, payload); err != nil {
            return err
        }
        pts += frameDur
    }
    return nil
}

// buildADTSHeader builds a 7-byte ADTS header (no CRC) for a single AAC LC frame.
func buildADTSHeader(sampleRate int, channels int, payloadLen int) []byte {
    // Map sample rate to ADTS index
    srTable := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
    srIndex := 4 // default 44100Hz
    for i, v := range srTable {
        if v == sampleRate {
            srIndex = i
            break
        }
    }
    if channels < 1 {
        channels = 2
    }
    profile := 1 // AAC LC (profile = objectType - 1), assume LC
    frameLen := payloadLen + 7

    hdr := make([]byte, 7)
    // syncword 0xFFF
    hdr[0] = 0xFF
    hdr[1] = 0xF1 // 1111 0001: sync high + MPEG-4 + layer 00 + no CRC
    hdr[2] = byte((profile&0x3)<<6 | (srIndex&0x0F)<<2 | (channels>>2)&0x1)
    hdr[3] = byte((channels&0x3)<<6 | ((frameLen>>11)&0x3))
    hdr[4] = byte((frameLen >> 3) & 0xFF)
    hdr[5] = byte(((frameLen & 0x7) << 5) | 0x1F)
    hdr[6] = 0xFC // 11111100: fullness and 0 raw blocks
    return hdr
}

// writeAudioPES writes raw audio PES payload (with ADTS already present) at given PTS.
func (e *mpegtsMuxer) writeAudioPES(pesData []byte, pts time.Duration) error {
    if e.aTrack == nil {
        return nil
    }
    mpegPts := durationGoToMPEGTS(pts)
    return e.w.writeAudio(e.aTrack, mpegPts, pesData)
}
