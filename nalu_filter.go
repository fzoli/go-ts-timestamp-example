package main

import (
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
)

// filterH265AU strips VPS/SPS/PPS/AUD NALUs out of au, storing the parameter
// sets into vps/sps/pps, and reports whether au contains an I-frame / IDR frame.
func filterH265AU(au [][]byte, vps, sps, pps *[]byte) (filtered [][]byte, isIFrame, isIDRFrame bool) {
	for _, nalu := range au {
		typ := h265.NALUType((nalu[0] >> 1) & 0b111111)
		switch typ {
		case h265.NALUType_VPS_NUT:
			*vps = nalu
			continue

		case h265.NALUType_SPS_NUT:
			*sps = nalu
			continue

		case h265.NALUType_PPS_NUT:
			*pps = nalu
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

		filtered = append(filtered, nalu)
	}
	return
}

// filterH264AU strips SPS/PPS/AUD NALUs out of au, storing the parameter
// sets into sps/pps, and reports whether au contains an IDR frame.
func filterH264AU(au [][]byte, sps, pps *[]byte) (filtered [][]byte, isIDRFrame bool) {
	for _, nalu := range au {
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeSPS:
			*sps = nalu
			continue
		case h264.NALUTypePPS:
			*pps = nalu
			continue
		case h264.NALUTypeAccessUnitDelimiter:
			continue
		case h264.NALUTypeIDR:
			isIDRFrame = true
		}
		filtered = append(filtered, nalu)
	}
	return
}
