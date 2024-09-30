package main

import (
	"bufio"
	"context"
	"github.com/asticode/go-astits"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/pkg/formats/mpegts"
	srt "github.com/datarhei/gosrt"
	"log"
	"os"
	"strconv"
	"time"
)

func testServer() {
	cfg := srt.DefaultConfig()
	cfg.SendBufferSize = bufferSize
	cfg.ReceiverBufferSize = bufferSize

	ln, err := srt.Listen("srt", ":6000", cfg)
	if err != nil {
		log.Fatalf("SRT server error: %v", err)
	}
	defer ln.Close()

	log.Println("Listening on port 6000")

	for {
		req, err := ln.Accept2()
		if err != nil {
			log.Printf("Connection error: %v", err)
			continue
		}

		go func(req srt.ConnRequest) {
			err := req.SetPassphrase("AES-encryption-passphrase")
			if err != nil {
				req.Reject(srt.REJ_PEER)
				log.Printf("Passphrase error: %v", err)
				return
			}

			if !isValidRequest(req) {
				req.Reject(srt.REJ_PEER)
				return
			}

			conn, err := req.Accept()
			if err != nil {
				log.Printf("Failed to accept the connection: %v", err)
				return
			}

			if isPublish(req) {
				handlePublish(conn)
			}
		}(req)
	}
}

func isValidRequest(req srt.ConnRequest) bool {
	return true // poc
}

func isPublish(req srt.ConnRequest) bool {
	return true // poc
}

func handlePublish(conn srt.Conn) {
	demuxer := astits.NewDemuxer(context.Background(), mpegts.NewBufferedReader(conn))
	var muxer *mpegtsMuxer
	for {
		data, err := demuxer.NextData()
		if err != nil {
			log.Println(err)
			break
		}
		if data.PES == nil {
			continue
		}
		au, ue := h264.AnnexBUnmarshal(data.PES.Data)
		if ue != nil {
			log.Println(ue)
			continue
		}
		var ntp time.Time
		hasNtp := false
		if data.PES.Header.OptionalHeader != nil && data.PES.Header.OptionalHeader.HasPrivateData {
			privateData := data.PES.Header.OptionalHeader.PrivateData
			var timestamp int64
			for i := uint(0); i < 8; i++ {
				timestamp |= int64(privateData[i+1]) << (8 * (7 - i))
			}
			ntp = time.UnixMilli(timestamp)
			hasNtp = true
			log.Printf("Time: %s", ntp)
		}
		if data.FirstPacket != nil && data.FirstPacket.AdaptationField != nil && data.FirstPacket.AdaptationField.RandomAccessIndicator {
			if muxer != nil {
				muxer.close()
			}
			var vps []byte
			var sps []byte
			var pps []byte
			for _, nalu := range au {
				typ := h265.NALUType((nalu[0] >> 1) & 0b111111)
				switch typ {
				case h265.NALUType_VPS_NUT:
					vps = nalu
				case h265.NALUType_SPS_NUT:
					sps = nalu
				case h265.NALUType_PPS_NUT:
					pps = nalu
				}
			}
			var tstime = ntp
			if !hasNtp {
				log.Println("Use system time")
				tstime = time.Now()
			}
			if len(vps) > 0 && len(sps) > 0 && len(pps) > 0 {
				file, err := os.Create("output-" + strconv.FormatInt(tstime.UnixMilli(), 10) + ".ts")
				if err != nil {
					log.Fatalf("Failed to create file: %v", err)
				}
				muxer = &mpegtsMuxer{
					vps: vps,
					sps: sps,
					pps: pps,
					b:   bufio.NewWriterSize(file, bufferSize),
				}
				err = muxer.initialize()
				if err != nil {
					panic(err)
				}
			} else {
				panic("missing codec params")
			}
		}
		if muxer != nil && data.PES != nil && data.PES.Header.OptionalHeader != nil && data.PES.Header.OptionalHeader.PTS != nil && data.PES.Header.IsVideoStream() {
			pts := data.PES.Header.OptionalHeader.PTS.Duration()
			werr := muxer.writeH265(au, pts, ntp, hasNtp) // TODO: use a simple muxer that uses the received DTS and does not create private data with timestamp
			if werr != nil {
				log.Println(werr)
			}
		}
	}
}
