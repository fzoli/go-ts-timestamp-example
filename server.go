package main

import (
    "bufio"
    "context"
    "log"
    "os"
    "strconv"
    "time"

    "github.com/asticode/go-astits"
    "github.com/bluenviron/mediacommon/pkg/codecs/h264"
    "github.com/bluenviron/mediacommon/pkg/codecs/h265"
    "github.com/bluenviron/mediacommon/pkg/formats/mpegts"
    srt "github.com/datarhei/gosrt"
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
    // Track PID -> StreamType from PMT to decide codec without guessing
    pidStreamType := map[uint16]astits.StreamType{}
    for {
        data, err := demuxer.NextData()
        if err != nil {
            log.Println(err)
            break
        }
        // Capture PMT to know which PIDs are H264/H265
        if data.PMT != nil {
            for _, es := range data.PMT.ElementaryStreams {
                pidStreamType[es.ElementaryPID] = es.StreamType
            }
            continue
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
            // Determine codec from PMT stream type
            st, ok := pidStreamType[data.PID]
            if !ok {
                log.Printf("Unknown stream type for PID %d; waiting for PMT", data.PID)
                continue
            }
            var vps []byte
            var sps []byte
            var pps []byte
            isH265 := st == astits.StreamTypeH265Video
            isH264 := st == astits.StreamTypeH264Video
            // Collect parameter sets from AU based on known codec
            if isH265 {
                for _, nalu := range au {
                    t := h265.NALUType((nalu[0] >> 1) & 0b111111)
                    switch t {
                    case h265.NALUType_VPS_NUT:
                        vps = nalu
                    case h265.NALUType_SPS_NUT:
                        sps = nalu
                    case h265.NALUType_PPS_NUT:
                        pps = nalu
                    }
                }
            } else if isH264 {
                for _, nalu := range au {
                    t := h264.NALUType(nalu[0] & 0x1F)
                    switch t {
                    case h264.NALUTypeSPS:
                        sps = nalu
                    case h264.NALUTypePPS:
                        pps = nalu
                    }
                }
            }
            var tstime = ntp
            if !hasNtp {
                log.Println("Use system time")
                tstime = time.Now()
            }
            file, err := os.Create("output-" + strconv.FormatInt(tstime.UnixMilli(), 10) + ".ts")
            if err != nil {
                log.Fatalf("Failed to create file: %v", err)
            }
            if isH265 {
                if len(vps) == 0 || len(sps) == 0 || len(pps) == 0 {
                    panic("missing H265 codec params")
                }
                muxer = &mpegtsMuxer{
                    vps:    vps,
                    sps:    sps,
                    pps:    pps,
                    isH265: true,
                    b:      bufio.NewWriterSize(file, bufferSize),
                }
            } else if isH264 {
                if len(sps) == 0 || len(pps) == 0 {
                    panic("missing H264 codec params")
                }
                muxer = &mpegtsMuxer{
                    sps:    sps,
                    pps:    pps,
                    isH265: false,
                    b:      bufio.NewWriterSize(file, bufferSize),
                }
            } else {
                panic("unknown codec in AU")
            }
            err = muxer.initialize()
            if err != nil {
                panic(err)
            }
        }
        if muxer != nil && data.PES != nil && data.PES.Header.OptionalHeader != nil && data.PES.Header.OptionalHeader.PTS != nil && data.PES.Header.IsVideoStream() {
            pts := data.PES.Header.OptionalHeader.PTS.Duration()
            var werr error
            if muxer.isH265 {
                werr = muxer.writeH265(au, pts, ntp, hasNtp)
            } else {
                werr = muxer.writeH264(au, pts, ntp, hasNtp)
            }
            if werr != nil {
                log.Println(werr)
            }
        }
    }
}
