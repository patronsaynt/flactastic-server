#ifndef CTAGLIB_SHIM_H
#define CTAGLIB_SHIM_H

/*
 * Shim header that forwards to the real TagLib C API header installed by
 * Homebrew. The `-I` flags supplied by `pkgConfig: "taglib_c"` put
 * <brew-prefix>/include (and .../include/taglib) on the search path, so the
 * include below resolves correctly without hard-coding the brew prefix.
 */
#include <taglib/tag_c.h>

#endif /* CTAGLIB_SHIM_H */
