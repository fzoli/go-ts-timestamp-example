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

	isRandomAccess := false

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

		case h265.NALUType_IDR_W_RADL, h265.NALUType_IDR_N_LP, h265.NALUType_CRA_NUT:
			isRandomAccess = true
		}

		filteredAU = append(filteredAU, nalu)
	}

	au = filteredAU

	if au == nil {
		return nil
	}

	// add VPS, SPS and PPS before random access access unit
	if isRandomAccess {
		au = append([][]byte{e.vps, e.sps, e.pps}, au...)
	}

	var dts time.Duration

	if e.dtsExtractor == nil {
		// skip samples silently until we find one with a IDR
		if !isRandomAccess {
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

	if isRandomAccess {
		packetTime := ntp
		if !hasNtp {
			packetTime = time.Now() // fallback to receiver system time
		}
		log.Printf("Write TS packet with pts=%d dts=%d time=%d", mpegPts, mpegDts, packetTime.UnixMilli())
		return e.w.WriteH265WithTimestamp(e.track, mpegPts, mpegDts, isRandomAccess, au, packetTime)
	} else {
		return e.w.WriteH265(e.track, mpegPts, mpegDts, isRandomAccess, au)
	}
}
