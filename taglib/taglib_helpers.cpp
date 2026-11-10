// Copied unmodified from the macOS app (Sources/CTagLibHelper); keep in sync.
#include "taglib_helpers.h"
#include <taglib/tag_c.h>
#include <taglib/fileref.h>
#include <taglib/audioproperties.h>
#include <taglib/flacproperties.h>
#include <taglib/mp4properties.h>
#include <taglib/wavproperties.h>
#include <taglib/aiffproperties.h>
#include <stdlib.h>
#include <string.h>

int taglib_helper_set_picture(void *file,
                              const char *data,
                              unsigned int size,
                              const char *mime) {
    if (!file || !data || size == 0 || !mime) return 0;
    TAGLIB_COMPLEX_PROPERTY_PICTURE(props, data, size, "", mime, "Front Cover");
    return taglib_complex_property_set((TagLib_File *)file, "PICTURE", props);
}

void taglib_helper_remove_pictures(void *file) {
    if (!file) return;
    taglib_complex_property_set((TagLib_File *)file, "PICTURE", NULL);
}

char *taglib_helper_get_album_artist(void *file) {
    if (!file) return NULL;
    char **values = taglib_property_get((TagLib_File *)file, "ALBUMARTIST");
    if (!values) return NULL;
    char *result = NULL;
    if (values[0] && values[0][0] != '\0') {
        size_t n = strlen(values[0]);
        result = (char *)malloc(n + 1);
        if (result) memcpy(result, values[0], n + 1);
    }
    taglib_property_free(values);
    return result;
}

void taglib_helper_set_album_artist(void *file, const char *value) {
    if (!file || !value) return;
    taglib_property_set((TagLib_File *)file, "ALBUMARTIST", value);
}

int taglib_helper_get_compilation(void *file) {
    if (!file) return 0;
    char **values = taglib_property_get((TagLib_File *)file, "COMPILATION");
    if (!values) return 0;
    int result = 0;
    if (values[0] && values[0][0] != '\0') {
        // Anything starting with '1' / 't' / 'T' / 'y' / 'Y' counts as truthy
        // — handles "1", "true", and the occasional "yes" written by other apps.
        char c = values[0][0];
        if (c == '1' || c == 't' || c == 'T' || c == 'y' || c == 'Y') result = 1;
    }
    taglib_property_free(values);
    return result;
}

void taglib_helper_set_compilation(void *file, int value) {
    if (!file) return;
    if (value) {
        taglib_property_set((TagLib_File *)file, "COMPILATION", "1");
    } else {
        // Empty value clears the property cross-format.
        taglib_property_set((TagLib_File *)file, "COMPILATION", "");
    }
}

void taglib_helper_set_lyrics(void *file, const char *value) {
    if (!file) return;
    // Empty / NULL value clears the property cross-format.
    taglib_property_set((TagLib_File *)file, "LYRICS",
                        (value && value[0] != '\0') ? value : "");
}

char *taglib_helper_get_lyrics(void *file) {
    if (!file) return NULL;
    char **values = taglib_property_get((TagLib_File *)file, "LYRICS");
    if (!values) return NULL;
    char *result = NULL;
    if (values[0] && values[0][0] != '\0') {
        size_t n = strlen(values[0]);
        result = (char *)malloc(n + 1);
        if (result) memcpy(result, values[0], n + 1);
    }
    taglib_property_free(values);
    return result;
}

int taglib_helper_get_mix_compilation(void *file) {
    if (!file) return 0;
    char **values = taglib_property_get((TagLib_File *)file, "MIXCOMPILATION");
    if (!values) return 0;
    int result = 0;
    if (values[0] && values[0][0] != '\0') {
        char c = values[0][0];
        if (c == '1' || c == 't' || c == 'T' || c == 'y' || c == 'Y') result = 1;
    }
    taglib_property_free(values);
    return result;
}

void taglib_helper_set_mix_compilation(void *file, int value) {
    if (!file) return;
    if (value) {
        taglib_property_set((TagLib_File *)file, "MIXCOMPILATION", "1");
    } else {
        taglib_property_set((TagLib_File *)file, "MIXCOMPILATION", "");
    }
}

char *taglib_helper_get_cuesheet(void *file) {
    if (!file) return NULL;
    char **values = taglib_property_get((TagLib_File *)file, "CUESHEET");
    if (!values) return NULL;
    char *result = NULL;
    if (values[0] && values[0][0] != '\0') {
        size_t n = strlen(values[0]);
        result = (char *)malloc(n + 1);
        if (result) memcpy(result, values[0], n + 1);
    }
    taglib_property_free(values);
    return result;
}

void taglib_helper_set_cuesheet(void *file, const char *value) {
    if (!file) return;
    taglib_property_set((TagLib_File *)file, "CUESHEET",
                        (value && value[0] != '\0') ? value : "");
}

char *taglib_helper_get_property(void *file, const char *key) {
    if (!file || !key) return NULL;
    char **values = taglib_property_get((TagLib_File *)file, key);
    if (!values) return NULL;
    char *result = NULL;
    if (values[0] && values[0][0] != '\0') {
        size_t n = strlen(values[0]);
        result = (char *)malloc(n + 1);
        if (result) memcpy(result, values[0], n + 1);
    }
    taglib_property_free(values);
    return result;
}

void taglib_helper_set_property(void *file, const char *key, const char *value) {
    if (!file || !key) return;
    taglib_property_set((TagLib_File *)file, key,
                        (value && value[0] != '\0') ? value : "");
}

unsigned char *taglib_helper_read_picture(void *file, unsigned int *out_size) {
    if (!file || !out_size) return NULL;
    *out_size = 0;

    TagLib_Complex_Property_Attribute ***props =
        taglib_complex_property_get((TagLib_File *)file, "PICTURE");
    if (!props) return NULL;

    unsigned char *result = NULL;
    for (int i = 0; props[i] != NULL; i++) {
        for (int j = 0; props[i][j] != NULL; j++) {
            TagLib_Complex_Property_Attribute *attr = props[i][j];
            if (strcmp(attr->key, "data") == 0 &&
                attr->value.type == TagLib_Variant_ByteVector &&
                attr->value.size > 0) {
                result = (unsigned char *)malloc(attr->value.size);
                if (result) {
                    memcpy(result, attr->value.value.byteVectorValue, attr->value.size);
                    *out_size = attr->value.size;
                }
                taglib_complex_property_free(props);
                return result;
            }
        }
    }
    taglib_complex_property_free(props);
    return NULL;
}

int taglib_helper_bits_per_sample(const char *path) {
    if (!path) return 0;
    TagLib::FileRef f(path, true, TagLib::AudioProperties::Average);
    if (f.isNull()) return 0;
    const TagLib::AudioProperties *props = f.audioProperties();
    if (!props) return 0;

    // bitsPerSample() lives on format-specific subclasses, not the base
    // AudioProperties — try the lossless containers AVFoundation can't read.
    if (auto p = dynamic_cast<const TagLib::FLAC::Properties *>(props)) return p->bitsPerSample();
    if (auto p = dynamic_cast<const TagLib::MP4::Properties *>(props)) return p->bitsPerSample();
    if (auto p = dynamic_cast<const TagLib::RIFF::WAV::Properties *>(props)) return p->bitsPerSample();
    if (auto p = dynamic_cast<const TagLib::RIFF::AIFF::Properties *>(props)) return p->bitsPerSample();
    return 0;
}
