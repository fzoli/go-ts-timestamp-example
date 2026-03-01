package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/asticode/go-astits"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/pkg/formats/mpegts"
	srt "github.com/datarhei/gosrt"
)

func testServer() {
	// Prepare HLS segmenter and HTTP server
	hls, err := NewHLSSegmenter("hls", 6)
	if err != nil {
		log.Fatalf("HLS init error: %v", err)
	}
	// Serve segments and playlist
	mux := http.NewServeMux()
	mux.Handle("/hls/", http.StripPrefix("/hls/", http.FileServer(http.Dir("hls"))))
	mux.HandleFunc("/hls/stream.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(hls.Playlist()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html>
<html><head><meta charset="utf-8"/><title>HLS Preview</title>
<script src="https://cdn.jsdelivr.net/npm/hls.js@latest"></script></head>
<body style="margin:0;background:#111;color:#eee;font-family:sans-serif;">
<div style="padding:8px;">/hls/stream.m3u8</div>
<video id="v" controls autoplay playsinline style="width:100%;max-width:960px;display:block;margin:0 auto;"></video>
<script>
const video = document.getElementById('v');
const src = '/hls/stream.m3u8';
if (video.canPlayType('application/vnd.apple.mpegurl')) { video.src = src; }
else if (Hls.isSupported()) { const hls = new Hls({lowLatencyMode:false}); hls.loadSource(src); hls.attachMedia(video); }
else { document.body.insertAdjacentHTML('beforeend','<p>No HLS support</p>'); }
</script></body></html>`))
	})
	go func() {
		log.Println("HTTP HLS on :8080 (GET / to preview)")
		if err := http.ListenAndServe(":8080", mux); err != nil {
			log.Printf("HTTP error: %v", err)
		}
	}()
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
				handlePublish(conn, hls)
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

func handlePublish(conn srt.Conn, hls *HLSSegmenter) {
	demuxer := astits.NewDemuxer(context.Background(), mpegts.NewBufferedReader(conn))
	// Track PID -> StreamType from PMT to decide codec without guessing
	pidStreamType := map[uint16]astits.StreamType{}
	var videoPID uint16
	var audioPID uint16
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
				if es.StreamType == astits.StreamTypeH264Video || es.StreamType == astits.StreamTypeH265Video {
					videoPID = es.ElementaryPID
				}
				if es.StreamType == astits.StreamTypeAACAudio {
					audioPID = es.ElementaryPID
				}
			}
			// Configure audio track presence once we know it's there
			if audioPID != 0 {
				// HLS TS expects ADTS; upstream PES already contains ADTS, we just enable the track
				hls.SetAudioParams(48000, 2, 2) // objectType=2 (AAC LC) as a safe default
			}
			continue
		}
		if data.PES == nil {
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
		if data.FirstPacket != nil && data.FirstPacket.AdaptationField != nil && data.FirstPacket.AdaptationField.RandomAccessIndicator && data.PID == videoPID {
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
			// Collect parameter sets from current video AU
			if isH265 || isH264 {
				au, ue := h264.AnnexBUnmarshal(data.PES.Data)
				if ue == nil {
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
					} else {
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
				}
			}
			// Configure codec params for HLS segmenter and start a new segment at this keyframe PTS
			hls.SetVideoParams(vps, sps, pps, isH265)
			if data.PES.Header.OptionalHeader == nil || data.PES.Header.OptionalHeader.PTS == nil {
				continue
			}
			firstPTS := data.PES.Header.OptionalHeader.PTS.Duration()
			if err := hls.StartSegment(firstPTS, func() time.Time {
				if hasNtp {
					return ntp
				}
				return time.Now()
			}()); err != nil {
				log.Printf("HLS start segment error: %v", err)
			}
		}
		// Write video PES
		if data.PES != nil && data.PES.Header.OptionalHeader != nil && data.PES.Header.OptionalHeader.PTS != nil && data.PID == videoPID {
			pts := data.PES.Header.OptionalHeader.PTS.Duration()
			// parse AnnexB AU for video only
			au, ue := h264.AnnexBUnmarshal(data.PES.Data)
			if ue == nil {
				st := pidStreamType[data.PID]
				isH265 := st == astits.StreamTypeH265Video
				if err := hls.WriteVideo(au, pts, ntp, hasNtp, isH265); err != nil {
					log.Println(err)
				}
			}
			continue
		}
		// Write audio PES as-is into current segment
		if data.PES != nil && data.PES.Header.OptionalHeader != nil && data.PES.Header.OptionalHeader.PTS != nil && data.PID == audioPID {
			pts := data.PES.Header.OptionalHeader.PTS.Duration()
			_ = hls.WriteAudioPES(data.PES.Data, pts)
		}
	}
	// finalize on exit
	_ = hls.CloseCurrent()
}
