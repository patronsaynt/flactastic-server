package meta

type Format string

const (
	FLAC Format = "flac"
	MP3  Format = "mp3"
	WAV  Format = "wav"
	AIFF Format = "aiff"
	ALAC Format = "alac"
	AAC  Format = "aac"
)

var Formats = []Format{FLAC, MP3, WAV, AIFF, ALAC, AAC}

// ClassifyExt is AudioFileFormat.classify(pathExtension:); m4a reads as ALAC.
func ClassifyExt(ext string) (Format, bool) {
	switch Lower(ext) {
	case "flac":
		return FLAC, true
	case "mp3":
		return MP3, true
	case "wav", "wave":
		return WAV, true
	case "aif", "aiff":
		return AIFF, true
	case "m4a":
		return ALAC, true
	case "aac":
		return AAC, true
	}
	return "", false
}
