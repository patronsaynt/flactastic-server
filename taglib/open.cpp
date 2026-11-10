#include "open.h"
#include <taglib/fileref.h>
#include <taglib/tfilestream.h>
#include <taglib/flacfile.h>
#include <taglib/mpegfile.h>
#include <taglib/mp4file.h>
#include <taglib/wavfile.h>
#include <taglib/aifffile.h>
#include <taglib/flacproperties.h>
#include <taglib/mp4properties.h>
#include <taglib/wavproperties.h>
#include <taglib/aiffproperties.h>

using namespace TagLib;

struct fts_handle {
    FileStream *stream;
    FileRef *ref;
};

// Opens an already-confined descriptor as the type its extension names, the
// same choice FileRef makes from a path. Takes ownership of fd.
fts_handle *fts_open(int fd, int type, int readOnly) {
    auto stream = new FileStream(fd, readOnly != 0);
    if (!stream->isOpen()) { delete stream; return nullptr; }
    File *file = nullptr;
    switch (type) {
    case FTS_FLAC: file = new FLAC::File(stream, true); break;
    case FTS_MPEG: file = new MPEG::File(stream, true); break;
    case FTS_MP4: file = new MP4::File(stream, true); break;
    case FTS_WAV: file = new RIFF::WAV::File(stream, true); break;
    case FTS_AIFF: file = new RIFF::AIFF::File(stream, true); break;
    }
    if (!file) { delete stream; return nullptr; }
    return new fts_handle{stream, new FileRef(file)};
}

TagLib_File *fts_file(fts_handle *h) { return reinterpret_cast<TagLib_File *>(h->ref); }

void fts_close(fts_handle *h) {
    delete h->ref;
    delete h->stream;
    delete h;
}

void fts_audio(fts_handle *h, fts_audio_t *out) {
    *out = fts_audio_t{};
    const AudioProperties *p = h->ref->audioProperties();
    if (!p) return;
    out->length_ms = p->lengthInMilliseconds();
    out->sample_rate = p->sampleRate();
    out->channels = p->channels();
    out->bitrate = p->bitrate();
    if (auto f = dynamic_cast<const FLAC::Properties *>(p)) {
        out->bits = f->bitsPerSample();
        out->frames = f->sampleFrames();
    } else if (auto m = dynamic_cast<const MP4::Properties *>(p)) {
        out->bits = m->bitsPerSample();
        out->codec = m->codec() == MP4::Properties::ALAC ? 2 : m->codec() == MP4::Properties::AAC ? 1 : 0;
    } else if (auto w = dynamic_cast<const RIFF::WAV::Properties *>(p)) {
        out->bits = w->bitsPerSample();
        out->frames = w->sampleFrames();
    } else if (auto a = dynamic_cast<const RIFF::AIFF::Properties *>(p)) {
        out->bits = a->bitsPerSample();
        out->frames = a->sampleFrames();
    }
}
