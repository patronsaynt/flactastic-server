#ifndef FTS_OPEN_H
#define FTS_OPEN_H

#include <taglib/tag_c.h>

#ifdef __cplusplus
extern "C" {
#endif

enum { FTS_FLAC = 1, FTS_MPEG, FTS_MP4, FTS_WAV, FTS_AIFF };

typedef struct fts_handle fts_handle;

typedef struct {
    int length_ms, sample_rate, channels, bitrate, bits;
    int codec; /* MP4 only: 1 AAC, 2 ALAC */
    unsigned long long frames;
} fts_audio_t;

fts_handle *fts_open(int fd, int type, int readOnly);
TagLib_File *fts_file(fts_handle *h);
void fts_close(fts_handle *h);
void fts_audio(fts_handle *h, fts_audio_t *out);

#ifdef __cplusplus
}
#endif

#endif
