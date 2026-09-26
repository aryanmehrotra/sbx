package fc

// macOS's lseek whences for sparse files: the same two as Linux's, with the numbers swapped.
const (
	seekHole = 3
	seekData = 4
)
