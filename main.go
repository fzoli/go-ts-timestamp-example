package main

import (
	"bufio"
	"log"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/codecs/h265"
	srt "github.com/datarhei/gosrt"
	"github.com/pion/rtp"
)

const (
	bufferSize = 1316 // SRT MPEG-TS buffer size
)

func main() {
	go testServer()

	transport := gortsplib.TransportTCP
	c := gortsplib.Client{
		Transport: &transport,
	}

	// parse URL
	u, err := base.ParseURL("rtsp://localhost:8554/h264src")
	if err != nil {
		panic(err)
	}

	// connect to the server
	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		panic(err)
	}
	defer c.Close()

	// find available medias
	desc, _, err := c.Describe(u)
	if err != nil {
		panic(err)
	}

	// find the H265 or H264 media and format
	var forma265 *format.H265
	var forma264 *format.H264
	medi := desc.FindFormat(&forma265)
	isH265 := true
	if medi == nil {
		medi = desc.FindFormat(&forma264)
		if medi == nil {
			panic("no H264/H265 media found")
		}
		isH265 = false
	}

	// setup RTP decoder
	var rtpDec interface{}
	if isH265 {
		rtpDec, err = forma265.CreateDecoder()
	} else {
		rtpDec, err = forma264.CreateDecoder()
	}
	if err != nil {
		panic(err)
	}

	// setup a single media
	_, err = c.Setup(desc.BaseURL, medi, 0, 0)
	if err != nil {
		panic(err)
	}

	// setup H265 -> MPEG-TS muxer
	var muxer *mpegtsMuxer

	// called when a RTP packet arrives
	if isH265 {
		dec := rtpDec.(*rtph265.Decoder)
		c.OnPacketRTP(medi, forma265, func(pkt *rtp.Packet) {
			// decode timestamp
			pts, ok := c.PacketPTS(medi, pkt)
			ntp, hasNtp := c.PacketNTP(medi, pkt)
			if !ok {
				log.Println("skip packet")
				return
			}

			// extract access unit from RTP packets
			au, err := dec.Decode(pkt)
			if err != nil {
				if err != rtph265.ErrNonStartingPacketAndNoPrevious && err != rtph265.ErrMorePacketsNeeded {
					// The above errors are not a real errors just signals to add more packet to the decoder.
					log.Printf("ERR: %v", err)
				}
				return
			}

			if muxer == nil {
				// Connect to the SRT server
				// Lazily so the server will not close the connection before the first packet arrives
				config := srt.DefaultConfig()
				config.StreamId = "publish:target"
				config.Passphrase = "AES-encryption-passphrase"
				config.SendBufferSize = bufferSize
				config.ReceiverBufferSize = bufferSize

				conn, err := srt.Dial("srt", "127.0.0.1:6000", config)
				if err != nil {
					panic(err)
				}
				muxer = &mpegtsMuxer{
					vps:    forma265.VPS,
					sps:    forma265.SPS,
					pps:    forma265.PPS,
					isH265: true,
					b:      bufio.NewWriterSize(conn, bufferSize),
				}

				var sps h265.SPS
				spsErr := sps.Unmarshal(forma265.SPS)
				if spsErr == nil {
					fps := sps.FPS()
					width := sps.Width()
					height := sps.Height()
					log.Printf("Video width: %d height: %d fps: %f", width, height, fps)
				}

				err = muxer.initialize()
				if err != nil {
					panic(err)
				}
			}

			// encode the access unit into MPEG-TS
			err = muxer.writeH265(au, pts, ntp, hasNtp)
			if err != nil {
				log.Printf("writeH265 ERR: %v", err)
				panic(err)
				return
			}
		})
	} else {
		dec := rtpDec.(*rtph264.Decoder)
		c.OnPacketRTP(medi, forma264, func(pkt *rtp.Packet) {
			// decode timestamp
			pts, ok := c.PacketPTS(medi, pkt)
			ntp, hasNtp := c.PacketNTP(medi, pkt)
			if !ok {
				log.Println("skip packet")
				return
			}

			// extract access unit from RTP packets
			au, err := dec.Decode(pkt)
			if err != nil {
				if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
					log.Printf("ERR: %v", err)
				}
				return
			}

			if muxer == nil {
				config := srt.DefaultConfig()
				config.StreamId = "publish:target"
				config.Passphrase = "AES-encryption-passphrase"
				config.SendBufferSize = bufferSize
				config.ReceiverBufferSize = bufferSize

				conn, err := srt.Dial("srt", "127.0.0.1:6000", config)
				if err != nil {
					panic(err)
				}
				muxer = &mpegtsMuxer{
					sps:    forma264.SPS,
					pps:    forma264.PPS,
					isH265: false,
					b:      bufio.NewWriterSize(conn, bufferSize),
				}

				var sps h264.SPS
				spsErr := sps.Unmarshal(forma264.SPS)
				if spsErr == nil {
					fps := sps.FPS()
					width := sps.Width()
					height := sps.Height()
					log.Printf("Video width: %d height: %d fps: %f", width, height, fps)
				}

				err = muxer.initialize()
				if err != nil {
					panic(err)
				}
			}

			// encode the access unit into MPEG-TS
			err = muxer.writeH264(au, pts, ntp, hasNtp)
			if err != nil {
				log.Printf("writeH264 ERR: %v", err)
				panic(err)
				return
			}
		})
	}

	// start playing
	_, err = c.Play(nil)
	if err != nil {
		panic(err)
	}

	// wait until a fatal error
	panic(c.Wait())
}
