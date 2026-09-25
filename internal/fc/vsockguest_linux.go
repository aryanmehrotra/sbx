package fc

// On linux the provider drives Firecracker itself (directly, or as the in-VM half of the
// helper-VM path on a Mac), so the vsock channel to execd is the one it uses.
func init() { NewGuest = func() Guest { return VsockGuest{} } }
