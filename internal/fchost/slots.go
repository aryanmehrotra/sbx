package fchost

import (
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// hostBusySlots is the slots whose public ports this machine already holds, as the in-VM
// provider's HostBusySlotsEnv wants them. A variable so GuestArgv is testable.
var hostBusySlots = func() string { return busySlots(provider.SlotPortsFree) }

// busySlots probes every slot a sandbox can have. The mirror binds a VM sandbox's ports at the
// same numbers here, so a slot the user's docker sandboxes hold on this Mac is one the VM must
// not hand out - it would come up unreachable, with the mirror failing to bind.
func busySlots(free func(slot int) bool) string {
	var held []string

	for s := range 128 {
		if !free(s) {
			held = append(held, strconv.Itoa(s))
		}
	}

	return strings.Join(held, ",")
}
