package main

import (
	"testing"

	"github.com/sirupsen/logrus"
)

func TestSocket(t *testing.T) {
	t.Parallel()

	logger.SetLevel(logrus.TraceLevel)

	fromVectorUploadChan := make(chan string, 5)
	uploadSockVectorSink = "./upload.sock"

	go listenOnSocketAndWriteToChan(uploadSockVectorSink, fromVectorUploadChan)

	for line := range fromVectorUploadChan {
		t.Logf("Received line: %s", line)
		if line == "exit" {
			break
		}
	}
	close(fromVectorUploadChan)
}
