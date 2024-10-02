package main

import (
	srt "github.com/datarhei/gosrt"
	"io"
	"log"
	"os/exec"
	"sync"
)

func testServer(wg *sync.WaitGroup) {
	defer wg.Done()

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
	cmd := exec.Command("ffplay", "-i", "pipe:0", "-probesize", "32", "-analyzeduration", "0")
	ffplayIn, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("Failed to create stdin pipe: %v", err)
	}
	defer ffplayIn.Close()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to get stdout: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalf("Failed to get stderr: %v", err)
	}

	err = cmd.Start()
	if err != nil {
		log.Fatalf("Failed to start ffplay: %v", err)
	}

	go io.Copy(log.Writer(), stdout)
	go io.Copy(log.Writer(), stderr)

	defer cmd.Cancel()

	buffer := make([]byte, bufferSize)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			log.Printf("Read error: %v", err)
			break
		}

		_, err = ffplayIn.Write(buffer[:n])
		if err != nil {
			log.Printf("Write error: %v", err)
			break
		}
	}
}
