package stream

import _ "embed"

// silenceMP3 is a ~1s silent clip at 128kbps/48kHz/stereo, matching the library's format.
//
//go:embed assets/silence.mp3
var silenceMP3 []byte
