package cluster

// Windows does not support fsync on directory handles opened by os.Open.
func syncCacheDirectory(string) error { return nil }
