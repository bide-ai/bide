package main

import "testing"

// The example must run to completion: each scene calls log.Fatalf (exiting the test binary,
// which fails the test) on any unexpected error, including a resume that is rejected.
func TestScenes(t *testing.T) {
	awaitScene()
	awaitForScene()
	channelScene()
}
