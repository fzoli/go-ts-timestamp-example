package main

import (
	"bufio"
	"log"
	"time"

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
	dtsExtractor *h265.DTSExtractor
}

// initialize initializes a mpegtsMuxer.
func (e *mpegtsMuxer) initialize() error {
	e.track = &TsTrack{
		Codec: &TsCodecH265{},
	}
	e.w = NewTsWriter(e.b, []*TsTrack{e.track})
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
			// ignore (will prepend later)
			continue

		case h265.NALUType_CRA_NUT, h265.NALUType_BLA_W_RADL, h265.NALUType_BLA_N_LP, h265.NALUType_BLA_W_LP:
			// CRA (and BLA) is an I-frame, but not an IDR so not a "real" random access point (zero fault tolerance)
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

	if e.dtsExtractor == nil {
		// skip samples silently until we find one with a IDR
		if !isIFrame {
			log.Printf("Do not send noise")
			return nil
		}
		e.dtsExtractor = h265.NewDTSExtractor()
	}

	var err error
	dts, err = e.dtsExtractor.Extract(au, pts)
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
