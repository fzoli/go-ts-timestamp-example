package main

import (
	"context"
	"github.com/asticode/go-astits"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"io"
	"time"
)

const (
	streamIDVideo = 224

	// PCR is needed to read H265 tracks with VLC+VDPAU hardware encoder
	// (and is probably needed by other combinations too)
	dtsPCRDiff         = 90000 / 10
	privateDataVersion = byte(1)
)

// NewTsWriter allocates a Writer.
func NewTsWriter(
	bw io.Writer,
	tracks []*TsTrack,
) *TsWriter {
	w := &TsWriter{
		nextPID: 256,
	}

	w.mux = astits.NewMuxer(
		context.Background(),
		bw)

	for _, track := range tracks {
		if track.PID == 0 {
			track.PID = w.nextPID
			w.nextPID++
		}
		es, _ := track.marshal()

		err := w.mux.AddElementaryStream(*es)
		if err != nil {
			panic(err) // TODO: return error instead of panicking
		}
	}

	// WriteTables() is not necessary
	// since it's called automatically when WriteData() is called with
	// * PID == PCRPID
	// * AdaptationField != nil
	// * RandomAccessIndicator = true

	return w
}

// TsWriter is a MPEG-TS writer.
type TsWriter struct {
	nextPID            uint16
	mux                *astits.Muxer
	pcrCounter         int
	leadingTrackChosen bool
}

type TsTrack struct {
	PID   uint16
	Codec TsCodec

	isLeading  bool // TsWriter-only
	mp3Checked bool // TsWriter-only
}

type TsCodec interface {
	marshal(pid uint16) (*astits.PMTElementaryStream, error)
}

// TsCodecH265 is a H265 codec.
type TsCodecH265 struct{}

func (c TsCodecH265) marshal(pid uint16) (*astits.PMTElementaryStream, error) {
	return &astits.PMTElementaryStream{
		ElementaryPID: pid,
		StreamType:    astits.StreamTypeH265Video,
	}, nil
}

func (t *TsTrack) marshal() (*astits.PMTElementaryStream, error) {
	return t.Codec.marshal(t.PID)
}

func (w *TsWriter) WriteH265(
	track *TsTrack,
	pts int64,
	dts int64,
	randomAccess bool,
	au [][]byte,
) error {
	// prepend an AUD. This is required by video.js, iOS, QuickTime
	if au[0][0] != byte(h265.NALUType_AUD_NUT<<1) {
		au = append([][]byte{
			{byte(h265.NALUType_AUD_NUT) << 1, 1, 0x50},
		}, au...)
	}

	enc, err := h264.AnnexBMarshal(au)
	if err != nil {
		return err
	}

	oh := &astits.PESOptionalHeader{
		MarkerBits: 2,
	}

	return w.writeVideo(track, pts, dts, randomAccess, enc, oh)
}

// WriteH265WithTimestamp writes a H265 access unit into MPEG-TS with a custom timestamp.
func (w *TsWriter) WriteH265WithTimestamp(
	track *TsTrack,
	pts int64,
	dts int64,
	randomAccess bool,
	au [][]byte,
	time time.Time,
) error {
	// prepend an AUD. This is required by video.js, iOS, QuickTime
	if au[0][0] != byte(h265.NALUType_AUD_NUT<<1) {
		au = append([][]byte{
			{byte(h265.NALUType_AUD_NUT) << 1, 1, 0x50},
		}, au...)
	}

	enc, err := h264.AnnexBMarshal(au)
	if err != nil {
		return err
	}

	// Include custom timestamp in the PES header for keyframes
	oh := &astits.PESOptionalHeader{
		MarkerBits: 2,
	}

	oh.HasPrivateData = true
	// (1)version + (8)timestamp
	oh.PrivateData = make([]byte, 9)
	// Set the version number in the first byte
	oh.PrivateData[0] = privateDataVersion
	// Fill the PrivateData field with the Unix timestamp (big-endian)
	timestamp := time.UnixMilli()
	for i := uint(0); i < 8; i++ {
		idx := i + 1 // index after the version number
		oh.PrivateData[idx] = byte((timestamp >> (8 * (7 - i))) & 0xFF)
	}

	// Write the video data with the custom PES header
	return w.writeVideo(track, pts, dts, randomAccess, enc, oh)
}

func (w *TsWriter) writeVideo(
	track *TsTrack,
	pts int64,
	dts int64,
	randomAccess bool,
	data []byte,
	oh *astits.PESOptionalHeader,
) error {
	if !w.leadingTrackChosen {
		w.leadingTrackChosen = true
		track.isLeading = true
		w.mux.SetPCRPID(track.PID)
	}

	var af *astits.PacketAdaptationField

	if randomAccess {
		af = &astits.PacketAdaptationField{}
		af.RandomAccessIndicator = true
	}

	if track.isLeading {
		if randomAccess || w.pcrCounter == 0 {
			if af == nil {
				af = &astits.PacketAdaptationField{}
			}
			af.HasPCR = true
			af.PCR = &astits.ClockReference{Base: dts - dtsPCRDiff}
			w.pcrCounter = 3
		}
		w.pcrCounter--
	}

	if dts == pts {
		oh.PTSDTSIndicator = astits.PTSDTSIndicatorOnlyPTS
		oh.PTS = &astits.ClockReference{Base: pts}
	} else {
		oh.PTSDTSIndicator = astits.PTSDTSIndicatorBothPresent
		oh.DTS = &astits.ClockReference{Base: dts}
		oh.PTS = &astits.ClockReference{Base: pts}
	}

	_, err := w.mux.WriteData(&astits.MuxerData{
		PID:             track.PID,
		AdaptationField: af,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: oh,
				StreamID:       streamIDVideo,
			},
			Data: data,
		},
	})
	return err
}
