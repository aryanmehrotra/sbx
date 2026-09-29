package procid

import "os"

func pid() int { return os.Getpid() }
