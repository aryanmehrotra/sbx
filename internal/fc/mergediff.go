package fc

// MergeDiff folds a Diff snapshot's memory file into the Full one it was taken on top of, in
// place, so the next restore loads a single file.
//
// Firecracker writes a Diff memory file sparse: only the pages dirtied since the VM was loaded
// are written, at their own offsets, and the rest is holes (spike: 128 MiB apparent, 1.4-2.1 MiB
// allocated). Merging is copying those written extents over the base - what the release's own
// rebase-snap tool does - and it has to be done by EXTENT, from SEEK_DATA/SEEK_HOLE, never by
// looking for non-zero bytes: a page the guest dirtied to all zeroes is data, and skipping it
// would restore the stale bytes underneath.
//
// Only ever called after the firecracker process that mapped base has been killed: a live VM
// maps its memory file MAP_PRIVATE, and writing under it would be a guest seeing its memory
// change.
func MergeDiff(diff, base string) error { return mergeDiff(diff, base) }
