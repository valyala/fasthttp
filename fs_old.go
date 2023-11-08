//go:build !go1.21
// +build !go1.21
package fasthttp

func (fs *FS) initRequestHandler() {
	root := fs.normalizeRoot(fs.Root)

	compressRoot := fs.CompressRoot
	if len(compressRoot) == 0 {
		compressRoot = root
	} else {
		compressRoot = fs.normalizeRoot(compressRoot)
	}

	compressedFileSuffixes := fs.CompressedFileSuffixes
	if len(compressedFileSuffixes["br"]) == 0 || len(compressedFileSuffixes["gzip"]) == 0 ||
		compressedFileSuffixes["br"] == compressedFileSuffixes["gzip"] {
		// Copy global map
		compressedFileSuffixes = make(map[string]string, len(FSCompressedFileSuffixes))
		for k, v := range FSCompressedFileSuffixes {
			compressedFileSuffixes[k] = v
		}
	}

	if len(fs.CompressedFileSuffix) > 0 {
		compressedFileSuffixes["gzip"] = fs.CompressedFileSuffix
		compressedFileSuffixes["br"] = FSCompressedFileSuffixes["br"]
	}

	h := &fsHandler{
		filesystem:             fs.FS,
		root:                   root,
		indexNames:             fs.IndexNames,
		pathRewrite:            fs.PathRewrite,
		generateIndexPages:     fs.GenerateIndexPages,
		compress:               fs.Compress,
		compressBrotli:         fs.CompressBrotli,
		compressRoot:           compressRoot,
		pathNotFound:           fs.PathNotFound,
		acceptByteRange:        fs.AcceptByteRange,
		compressedFileSuffixes: compressedFileSuffixes,
	}

	h.cacheManager = newCacheManager(fs)

	if h.filesystem == nil {
		h.filesystem = &osFS{} // It provides os.Open and os.Stat
	}

	fs.h = h.handleRequest
}